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
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
)

// ErrorBody is the one error model for the management API: a stable code, a
// human message, and the request id that ties the response to a log line.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	// Missing names the evidence types a refused promotion lacks
	// (GATE_FAILED only).
	Missing []string `json:"missing,omitempty"`
	// Details locates each problem in a refused configuration, by file,
	// line, and field.
	Details []*config.FieldError `json:"details,omitempty"`
}

// Options wires the management API.
type Options struct {
	Registry *registry.Registry
	Auth     auth.Authenticator
	// Lifecycle and Evidence serve the subject, promotion, and decision
	// routes. Both or neither: nil leaves those routes unmounted, which only
	// tests of other surfaces do; serve always wires them.
	Lifecycle *lifecycle.Service
	Evidence  *evidence.Service
	// RequestID extracts the request id set by the server middleware.
	RequestID func(context.Context) string
	// Credentials lists the credentials references the server defines, so a
	// repository naming an unknown one is refused when it is created (ADR 15).
	// Nil means none are defined.
	Credentials func() map[string]struct{}
	// Reconciler serves /config/diff and /config/apply. Nil leaves them
	// unmounted.
	Reconciler *reconcile.Reconciler
	// Tokens serves /tokens. Nil leaves it unmounted.
	Tokens *auth.TokenService
	// GC and Queue serve /gc and /jobs. Both or neither.
	GC    *gc.Collector
	Queue *queue.Queue
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
	if opts.Credentials == nil {
		opts.Credentials = func() map[string]struct{} { return nil }
	}

	if (opts.Lifecycle == nil) != (opts.Evidence == nil) {
		panic("api.Handler: Lifecycle and Evidence must be set together")
	}
	if (opts.GC == nil) != (opts.Queue == nil) {
		panic("api.Handler: GC and Queue must be set together")
	}

	a := &api{
		reg:         opts.Registry,
		lifecycle:   opts.Lifecycle,
		evidence:    opts.Evidence,
		auth:        opts.Auth,
		requestID:   opts.RequestID,
		credentials: opts.Credentials,
		reconciler:  opts.Reconciler,
		tokens:      opts.Tokens,
		gc:          opts.GC,
		queue:       opts.Queue,
	}

	r := chi.NewRouter()
	r.Use(auth.Middleware(a.auth, a.denyUnauthenticated))

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

	if a.lifecycle != nil {
		a.mountIdentity(r)
	}
	if a.reconciler != nil {
		r.Post("/config/diff", a.configDiff)
		r.Post("/config/apply", a.configApply)
	}
	if a.tokens != nil {
		r.Get("/tokens", a.listTokens)
		r.Post("/tokens", a.createToken)
		r.Delete("/tokens/{id}", a.revokeToken)
	}
	if a.gc != nil {
		r.Get("/gc/candidates", a.gcCandidates)
		r.Post("/gc", a.startGC)
		r.Get("/jobs/{id}", a.getJob)
	}

	return r
}

type api struct {
	reg         *registry.Registry
	lifecycle   *lifecycle.Service
	evidence    *evidence.Service
	auth        auth.Authenticator
	requestID   func(context.Context) string
	credentials func() map[string]struct{}
	reconciler  *reconcile.Reconciler
	tokens      *auth.TokenService
	gc          *gc.Collector
	queue       *queue.Queue
}

func (a *api) createRepository(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "create repositories") {
		return
	}
	var body struct {
		Name               string   `json:"name"`
		Type               string   `json:"type"`
		Protocol           string   `json:"protocol"`
		UpstreamURL        string   `json:"upstream_url"`
		CredentialsRef     string   `json:"credentials_ref"`
		TTLSeconds         int32    `json:"ttl_seconds"`
		NegativeTTLSeconds int32    `json:"negative_ttl_seconds"`
		Members            []string `json:"members"`
		LifecycleStages    []string `json:"lifecycle_stages"`
		Read               []string `json:"read"`
		Write              []string `json:"write"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be JSON")
		return
	}
	repoType := model.RepoType(body.Type)
	if repoType == "" {
		repoType = model.RepoTypeHosted
	}
	params := registry.CreateRepositoryParams{
		Name:            body.Name,
		Type:            repoType,
		Protocol:        model.Protocol(body.Protocol),
		UpstreamURL:     body.UpstreamURL,
		CredentialsRef:  body.CredentialsRef,
		TTLSeconds:      body.TTLSeconds,
		NegativeTTL:     body.NegativeTTLSeconds,
		Members:         body.Members,
		LifecycleStages: body.LifecycleStages,
		Access:          model.Access{Read: body.Read, Write: body.Write},
	}
	if err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name:            params.Name,
		Type:            params.Type,
		Protocol:        params.Protocol,
		UpstreamURL:     params.UpstreamURL,
		CredentialsRef:  params.CredentialsRef,
		Members:         params.Members,
		LifecycleStages: params.LifecycleStages,
	}, a.credentials()); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	repo, err := a.reg.CreateRepository(r.Context(), params)
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
	// A page lists only what the caller may read; the cursor still walks
	// every name, so paging stays stable as grants change.
	p := principal(r)
	items := make([]repositoryResponse, 0, len(page.Items))
	for _, repo := range page.Items {
		if auth.Authorize(p, repo, auth.RoleRead) == nil {
			items = append(items, repositoryJSON(repo))
		}
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[repositoryResponse]{
		Items:      items,
		NextCursor: page.NextCursor,
	})
}

func (a *api) getRepository(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.requireRepo(w, r, chi.URLParam(r, "name"), auth.RoleRead)
	if !ok {
		return
	}
	a.writeJSON(w, r, http.StatusOK, repositoryJSON(repo))
}

func (a *api) deleteRepository(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "delete repositories") {
		return
	}
	if err := a.reg.DeleteRepository(r.Context(), chi.URLParam(r, "name")); err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listPackages(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireRepo(w, r, chi.URLParam(r, "repo"), auth.RoleRead); !ok {
		return
	}
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
	if _, ok := a.requireRepo(w, r, chi.URLParam(r, "repo"), auth.RoleRead); !ok {
		return
	}
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
	if _, ok := a.requireRepo(w, r, chi.URLParam(r, "repo"), auth.RoleWrite); !ok {
		return
	}
	mediaType := r.Header.Get("Content-Type")
	size := int64(-1)
	if cl := r.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}

	file, err := a.reg.Publish(r.Context(), registry.PublishParams{
		Repo:       chi.URLParam(r, "repo"),
		Package:    chi.URLParam(r, "name"),
		Version:    chi.URLParam(r, "version"),
		Filename:   chi.URLParam(r, "filename"),
		MediaType:  mediaType,
		Body:       r.Body,
		Size:       size,
		Provenance: model.HeaderProvenance(model.ProvenanceAPIPublish, r.Header.Get),
	})
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, fileJSON(file))
}

func (a *api) getFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireRepo(w, r, chi.URLParam(r, "repo"), auth.RoleRead); !ok {
		return
	}
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
	if _, ok := a.requireRepo(w, r, chi.URLParam(r, "repo"), auth.RoleWrite); !ok {
		return
	}
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
	case errors.Is(err, registry.ErrHasHistory):
		a.writeError(w, r, http.StatusConflict, "HAS_HISTORY", err.Error())
	case errors.Is(err, registry.ErrConfigManaged):
		a.writeError(w, r, http.StatusConflict, "CONFIG_MANAGED", err.Error())
	case errors.Is(err, registry.ErrInvalidInput), errors.Is(err, registry.ErrCycle), errors.Is(err, registry.ErrReadOnly):
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
	Name      string       `json:"name"`
	Type      string       `json:"type"`
	Protocol  string       `json:"protocol"`
	Locked    bool         `json:"locked"`
	ManagedBy string       `json:"managed_by"`
	CreatedAt string       `json:"created_at"`
	Namespace string       `json:"namespace,omitempty"`
	Access    model.Access `json:"access"`
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
		Namespace: repo.Namespace,
		Access:    repo.Access,
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
