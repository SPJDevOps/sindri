// Package api is the management HTTP surface under /api/v1.
//
// Handlers translate HTTP into registry calls and map domain errors onto the
// stable error model. Domain logic stays in registry; authentication stays
// behind auth.Authenticator so M7 can swap the implementation without
// rewriting these routes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// ErrorBody is the one error model for the management API: a stable code, a
// human message, and the request id that ties the response to a log line.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// Options wires the management API.
type Options struct {
	Registry *registry.Registry
	Auth     auth.Authenticator
	// RequestID extracts the request id set by the server middleware.
	RequestID func(context.Context) string
}

// Handler returns the /api/v1 chi router. Mount it with r.Mount("/api/v1", ...).
func Handler(opts Options) http.Handler {
	if opts.Registry == nil {
		panic("api.Handler: nil registry")
	}
	if opts.Auth == nil {
		panic("api.Handler: nil authenticator")
	}
	if opts.RequestID == nil {
		opts.RequestID = func(context.Context) string { return "" }
	}

	a := &api{reg: opts.Registry, auth: opts.Auth, requestID: opts.RequestID}

	r := chi.NewRouter()
	r.Use(a.requireAuth)

	r.Route("/repositories", func(r chi.Router) {
		r.Get("/", a.listRepositories)
		r.Post("/", a.createRepository)
		r.Get("/{name}", a.getRepository)
		r.Delete("/{name}", a.deleteRepository)

		r.Get("/{repo}/packages", a.listPackages)
		r.Get("/{repo}/packages/{name}/versions", a.listVersions)
		r.Put("/{repo}/packages/{name}/versions/{version}/files/{filename}", a.publishFile)
		r.Get("/{repo}/packages/{name}/versions/{version}/files/{filename}", a.getFile)
		r.Delete("/{repo}/packages/{name}/versions/{version}/files/{filename}", a.deleteFile)
	})

	return r
}

type api struct {
	reg       *registry.Registry
	auth      auth.Authenticator
	requestID func(context.Context) string
}

func (a *api) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.auth.Authenticate(r.Context(), r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sindri"`)
			a.writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "bearer token required")
			return
		}
		ctx := context.WithValue(r.Context(), identityCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type identityCtxKey struct{}

func (a *api) createRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be JSON")
		return
	}
	repoType := model.RepoType(body.Type)
	if repoType == "" {
		repoType = model.RepoTypeHosted
	}
	repo, err := a.reg.CreateRepository(r.Context(), registry.CreateRepositoryParams{
		Name:     body.Name,
		Type:     repoType,
		Protocol: model.Protocol(body.Protocol),
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, repositoryJSON(repo))
}

func (a *api) listRepositories(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	page, err := a.reg.ListRepositories(r.Context(), registry.ListRepositoriesParams{
		After: r.URL.Query().Get("cursor"),
		Limit: limit,
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	items := make([]repositoryResponse, 0, len(page.Items))
	for _, repo := range page.Items {
		items = append(items, repositoryJSON(repo))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[repositoryResponse]{
		Items:      items,
		NextCursor: page.NextCursor,
	})
}

func (a *api) getRepository(w http.ResponseWriter, r *http.Request) {
	repo, err := a.reg.GetRepository(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, repositoryJSON(repo))
}

func (a *api) deleteRepository(w http.ResponseWriter, r *http.Request) {
	if err := a.reg.DeleteRepository(r.Context(), chi.URLParam(r, "name")); err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listPackages(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	page, err := a.reg.ListPackages(r.Context(), registry.ListPackagesParams{
		Repo:  chi.URLParam(r, "repo"),
		After: r.URL.Query().Get("cursor"),
		Limit: limit,
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	items := make([]packageResponse, 0, len(page.Items))
	for _, pkg := range page.Items {
		items = append(items, packageJSON(pkg))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[packageResponse]{
		Items:      items,
		NextCursor: page.NextCursor,
	})
}

func (a *api) listVersions(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	page, err := a.reg.ListVersions(r.Context(), registry.ListVersionsParams{
		Repo:    chi.URLParam(r, "repo"),
		Package: chi.URLParam(r, "name"),
		After:   r.URL.Query().Get("cursor"),
		Limit:   limit,
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	items := make([]versionResponse, 0, len(page.Items))
	for _, ver := range page.Items {
		items = append(items, versionJSON(ver))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[versionResponse]{
		Items:      items,
		NextCursor: page.NextCursor,
	})
}

func (a *api) publishFile(w http.ResponseWriter, r *http.Request) {
	mediaType := r.Header.Get("Content-Type")
	size := int64(-1)
	if cl := r.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}

	file, err := a.reg.Publish(r.Context(), registry.PublishParams{
		Repo:      chi.URLParam(r, "repo"),
		Package:   chi.URLParam(r, "name"),
		Version:   chi.URLParam(r, "version"),
		Filename:  chi.URLParam(r, "filename"),
		MediaType: mediaType,
		Body:      r.Body,
		Size:      size,
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, fileJSON(file))
}

func (a *api) getFile(w http.ResponseWriter, r *http.Request) {
	file, rc, err := a.reg.GetFile(r.Context(),
		chi.URLParam(r, "repo"),
		chi.URLParam(r, "name"),
		chi.URLParam(r, "version"),
		chi.URLParam(r, "filename"),
	)
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", file.MediaType)
	w.Header().Set("Docker-Content-Digest", file.Digest.String())
	w.Header().Set("X-Sindri-Digest", file.Digest.String())
	if file.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already sent; the access log still records the failure
		// via the server middleware when the connection drops.
		return
	}
}

func (a *api) deleteFile(w http.ResponseWriter, r *http.Request) {
	err := a.reg.DeleteFile(r.Context(),
		chi.URLParam(r, "repo"),
		chi.URLParam(r, "name"),
		chi.URLParam(r, "version"),
		chi.URLParam(r, "filename"),
	)
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) writeRegistryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrNotPlaced):
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, registry.ErrAlreadyExists):
		a.writeError(w, r, http.StatusConflict, "ALREADY_EXISTS", "resource already exists")
	case errors.Is(err, registry.ErrInvalidInput):
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

func (a *api) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	a.writeJSON(w, r, status, ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: a.requestID(r.Context()),
	})
}

func (a *api) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func parseLimit(r *http.Request) (int32, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}
	return int32(n), nil
}

type listResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type repositoryResponse struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Protocol  string `json:"protocol"`
	Locked    bool   `json:"locked"`
	ManagedBy string `json:"managed_by"`
	CreatedAt string `json:"created_at"`
}

type packageResponse struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type versionResponse struct {
	Version string `json:"version"`
}

type fileResponse struct {
	Filename  string `json:"filename"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

func repositoryJSON(repo model.Repository) repositoryResponse {
	return repositoryResponse{
		Name:      repo.Name,
		Type:      string(repo.Type),
		Protocol:  string(repo.Protocol),
		Locked:    repo.Locked,
		ManagedBy: string(repo.ManagedBy),
		CreatedAt: repo.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func packageJSON(pkg model.Package) packageResponse {
	return packageResponse{Name: pkg.NormalizedName, DisplayName: pkg.DisplayName}
}

func versionJSON(ver model.Version) versionResponse {
	return versionResponse{Version: ver.Version}
}

func fileJSON(file model.File) fileResponse {
	return fileResponse{
		Filename:  file.Filename,
		Digest:    file.Digest.String(),
		MediaType: file.MediaType,
		Size:      file.Size,
	}
}
