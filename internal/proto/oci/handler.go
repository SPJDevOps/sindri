package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/storage"
)

// Options wires the OCI adapter.
type Options struct {
	Resolver *resolve.Resolver
	CAS      *cas.Store
}

// Handler returns the /v2 handler. Mount it with r.Mount("/v2", ...).
//
// A plain handler is used instead of chi path params because OCI repository
// names contain slashes mid-path (e.g. demo/app/blobs/...), which no fixed
// segment pattern can express.
func Handler(opts Options) http.Handler {
	if opts.Resolver == nil {
		panic("oci.Handler: nil resolver")
	}
	if opts.CAS == nil {
		panic("oci.Handler: nil cas")
	}
	h := &handler{res: opts.Resolver, reg: opts.Resolver.Registry(), cas: opts.CAS}
	return http.HandlerFunc(h.serve)
}

type handler struct {
	res *resolve.Resolver
	reg *registry.Registry
	cas *cas.Store
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	// chi.Mount strips /v2 for nested routers; a plain handler may still see
	// the full path. Accept both.
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/v2")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
			return
		}
		h.versionCheck(w, r)
		return
	}
	h.dispatch(w, r, path)
}

func (h *handler) versionCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusOK)
}

func (h *handler) dispatch(w http.ResponseWriter, r *http.Request, path string) {
	repo, image, rest, err := h.res.SplitOCIPath(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
		return
	}
	// clientPath is the original name clients address (repo + image), used in
	// Location headers so docker keeps talking to the same path.
	clientPath := repo.Name
	if image != "" {
		clientPath = repo.Name + "/" + image
	}

	switch {
	case rest == "tags/list":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
			return
		}
		h.listTags(w, r, repo, image, clientPath)
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		if ref == "" {
			writeError(w, http.StatusBadRequest, CodeManifestInvalid, "manifest reference required")
			return
		}
		h.handleManifest(w, r, repo, image, clientPath, ref)
	case strings.HasPrefix(rest, "blobs/uploads"):
		h.handleUpload(w, r, repo, image, clientPath, rest)
	case strings.HasPrefix(rest, "blobs/"):
		digestStr := strings.TrimPrefix(rest, "blobs/")
		h.handleBlob(w, r, repo, image, clientPath, digestStr)
	default:
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
	}
}

// splitRepoPath splits "{name}/(blobs|manifests|tags)/..." where name may
// contain slashes. Longest-prefix matching against configured repositories is
// done by resolve.SplitOCIPath; this helper only finds the route marker.
func splitRepoPath(path string) (name, rest string, ok bool) {
	for _, marker := range []string{"/blobs/", "/manifests/", "/tags/"} {
		if i := strings.Index(path, marker); i > 0 {
			return path[:i], path[i+1:], true
		}
	}
	if i := strings.Index(path, "/blobs/uploads"); i > 0 {
		return path[:i], path[i+1:], true
	}
	return "", "", false
}

func (h *handler) handleBlob(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath, digestStr string) {
	digest, err := model.ParseDigest(digestStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeDigestInvalid, "invalid digest")
		return
	}

	switch r.Method {
	case http.MethodHead, http.MethodGet:
		h.getBlob(w, r, repo, image, digest)
	case http.MethodDelete:
		writeRepo, _, err := h.ensureWritable(r.Context(), repo, image, clientPath)
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		if err := h.reg.UnlinkBlob(r.Context(), writeRepo, digest); err != nil {
			mapBlobReadError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
	}
}

func (h *handler) getBlob(w http.ResponseWriter, r *http.Request, repo model.Repository, image string, digest model.Digest) {
	rangeHeader := r.Header.Get("Range")
	if r.Method == http.MethodHead || rangeHeader != "" {
		h.statOrRangeBlob(w, r, repo, image, digest, rangeHeader)
		return
	}

	// A plain GET does not stat first: for an uncached proxy blob that would
	// be a second upstream round trip, and the fetch already knows the size.
	rc, size, err := h.res.GetOCIBlob(r.Context(), repo.Name, image, digest)
	if err != nil {
		mapBlobReadError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	setBlobHeaders(w, digest)
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are sent, so the only honest signal left is to cut the
		// connection: a clean end would let a client keep bytes that failed
		// verification or stopped short. net/http treats this panic value as
		// "abort the response" and the recoverer passes it through.
		panic(http.ErrAbortHandler)
	}
}

func setBlobHeaders(w http.ResponseWriter, digest model.Digest) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest.String())
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
}

// statOrRangeBlob answers HEAD without downloading anything, and a ranged GET
// from storage once the blob is cached.
func (h *handler) statOrRangeBlob(w http.ResponseWriter, r *http.Request, repo model.Repository, image string, digest model.Digest, rangeHeader string) {
	size, err := h.res.StatOCIBlob(r.Context(), repo.Name, image, digest)
	if err != nil {
		mapBlobReadError(w, err)
		return
	}
	setBlobHeaders(w, digest)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	off, length, ok := parseByteRange(rangeHeader, size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, CodeSizeInvalid, "invalid range")
		return
	}
	rc, err := h.res.GetOCIBlobRange(r.Context(), repo.Name, image, digest, off, length)
	if err != nil {
		mapBlobReadError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	end := off + length - 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.Copy(w, rc)
}

// parseByteRange parses "bytes=start-end" into offset and length.
func parseByteRange(h string, size int64) (off, length int64, ok bool) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if startStr == "" {
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if endStr == "" {
		return start, size - start, true
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}

func (h *handler) handleUpload(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath, rest string) {
	rest = strings.TrimSuffix(rest, "/")
	switch {
	case rest == "blobs/uploads":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
			return
		}
		h.startUpload(w, r, repo, image, clientPath)
	case strings.HasPrefix(rest, "blobs/uploads/"):
		id := storage.UploadID(strings.TrimPrefix(rest, "blobs/uploads/"))
		if id == "" {
			writeError(w, http.StatusNotFound, CodeBlobUploadUnknown, "blob upload unknown to registry")
			return
		}
		switch r.Method {
		case http.MethodPatch:
			h.patchUpload(w, r, clientPath, id)
		case http.MethodPut:
			h.putUpload(w, r, repo, image, clientPath, id)
		case http.MethodGet:
			h.getUploadStatus(w, r, clientPath, id)
		case http.MethodDelete:
			h.abortUpload(w, r, clientPath, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, CodeBlobUploadUnknown, "blob upload unknown to registry")
	}
}

func uploadLocation(name string, id storage.UploadID) string {
	return "/v2/" + name + "/blobs/uploads/" + string(id)
}

func inclusiveRangeHeader(size int64) string {
	if size <= 0 {
		return "0-0"
	}
	return "0-" + strconv.FormatInt(size-1, 10)
}

// ensureWritable returns the Sindri repository name and image package for a
// write. Proxy and virtual repositories are rejected. When no repository row
// matched the path, the full client path is auto-created as hosted (M3).
func (h *handler) ensureWritable(ctx context.Context, repo model.Repository, image, clientPath string) (repoName, imageName string, err error) {
	if repo.ID != "" {
		if repo.Type != model.RepoTypeHosted {
			return "", "", fmt.Errorf("%w: %s is %s", registry.ErrReadOnly, repo.Name, repo.Type)
		}
		return repo.Name, image, nil
	}
	created, err := h.reg.EnsureOCIRepository(ctx, clientPath)
	if err != nil {
		return "", "", err
	}
	return created.Name, "", nil
}

func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath string) {
	writeRepo, _, err := h.ensureWritable(r.Context(), repo, image, clientPath)
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	digestStr := r.URL.Query().Get("digest")
	if digestStr != "" {
		digest, err := model.ParseDigest(digestStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeDigestInvalid, "invalid digest")
			return
		}
		upload, err := h.cas.NewUpload(r.Context())
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		if _, err := upload.Append(r.Context(), r.Body); err != nil {
			_ = upload.Abort(r.Context())
			mapRegistryError(w, err)
			return
		}
		info, err := h.reg.CommitBlobUpload(r.Context(), writeRepo, upload, digest)
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		w.Header().Set("Docker-Content-Digest", info.Digest.String())
		w.Header().Set("Location", "/v2/"+clientPath+"/blobs/"+info.Digest.String())
		w.Header().Set("Docker-Distribution-API-Version", apiVersion)
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Cross-repository mount (?mount=&from=) is out of scope for M3. Per the
	// distribution spec, a failed mount falls back to a fresh upload session.
	if r.URL.Query().Get("mount") != "" {
		upload, err := h.cas.NewUpload(r.Context())
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		w.Header().Set("Location", uploadLocation(clientPath, upload.ID()))
		w.Header().Set("Range", inclusiveRangeHeader(0))
		w.Header().Set("Docker-Upload-UUID", string(upload.ID()))
		w.Header().Set("Docker-Distribution-API-Version", apiVersion)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	upload, err := h.cas.NewUpload(r.Context())
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Location", uploadLocation(clientPath, upload.ID()))
	w.Header().Set("Range", inclusiveRangeHeader(0))
	w.Header().Set("Docker-Upload-UUID", string(upload.ID()))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusAccepted)
}

func (h *handler) patchUpload(w http.ResponseWriter, r *http.Request, clientPath string, id storage.UploadID) {
	upload, err := h.cas.ResumeUpload(id)
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	// Content-Range is optional; when present the start must equal the current
	// size. Out-of-order or duplicate chunks are 416 (inclusive Range stays
	// at the previous end).
	if cr := r.Header.Get("Content-Range"); cr != "" {
		start, ok := parseContentRangeStart(cr)
		if !ok || start != upload.Size() {
			w.Header().Set("Range", inclusiveRangeHeader(upload.Size()))
			w.Header().Set("Location", uploadLocation(clientPath, id))
			w.Header().Set("Docker-Upload-UUID", string(id))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	if _, err := upload.Append(r.Context(), r.Body); err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Location", uploadLocation(clientPath, id))
	w.Header().Set("Range", inclusiveRangeHeader(upload.Size()))
	w.Header().Set("Docker-Upload-UUID", string(id))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusAccepted)
}

// parseContentRangeStart extracts the start offset from an OCI chunked-upload
// Content-Range value. The distribution spec uses "start-end" (inclusive), not
// the HTTP "bytes start-end/size" form — though both are accepted.
func parseContentRangeStart(h string) (int64, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	lower := strings.ToLower(h)
	if strings.HasPrefix(lower, "bytes ") {
		h = strings.TrimSpace(h[len("bytes "):])
	}
	rangePart, _, _ := strings.Cut(h, "/")
	startStr, _, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

func (h *handler) putUpload(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath string, id storage.UploadID) {
	writeRepo, _, err := h.ensureWritable(r.Context(), repo, image, clientPath)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	digestStr := r.URL.Query().Get("digest")
	digest, err := model.ParseDigest(digestStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeDigestInvalid, "invalid digest")
		return
	}
	upload, err := h.cas.ResumeUpload(id)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if r.Body != nil {
		if _, err := upload.Append(r.Context(), r.Body); err != nil {
			mapRegistryError(w, err)
			return
		}
	}
	info, err := h.reg.CommitBlobUpload(r.Context(), writeRepo, upload, digest)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Docker-Content-Digest", info.Digest.String())
	w.Header().Set("Location", "/v2/"+clientPath+"/blobs/"+info.Digest.String())
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) getUploadStatus(w http.ResponseWriter, r *http.Request, clientPath string, id storage.UploadID) {
	upload, err := h.cas.ResumeUpload(id)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Range", inclusiveRangeHeader(upload.Size()))
	w.Header().Set("Docker-Upload-UUID", string(id))
	w.Header().Set("Location", uploadLocation(clientPath, id))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) abortUpload(w http.ResponseWriter, r *http.Request, clientPath string, id storage.UploadID) {
	upload, err := h.cas.ResumeUpload(id)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if err := upload.Abort(r.Context()); err != nil {
		mapRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleManifest(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, repo, image, ref)
	case http.MethodPut:
		h.putManifest(w, r, repo, image, clientPath, ref)
	case http.MethodDelete:
		h.deleteManifest(w, r, repo, image, clientPath, ref)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
	}
}

func (h *handler) getManifest(w http.ResponseWriter, r *http.Request, repo model.Repository, image, ref string) {
	accept := r.Header.Get("Accept")
	man, err := h.res.GetOCIManifest(r.Context(), repo.Name, image, ref, accept)
	if err != nil {
		mapManifestReadError(w, err)
		return
	}
	w.Header().Set("Content-Type", man.MediaType)
	w.Header().Set("Docker-Content-Digest", man.Digest.String())
	w.Header().Set("Content-Length", strconv.FormatInt(man.Size, 10))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(man.Body)
}

func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath, ref string) {
	writeRepo, writeImage, err := h.ensureWritable(r.Context(), repo, image, clientPath)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeManifestInvalid, "could not read body")
		return
	}
	mediaType := r.Header.Get("Content-Type")
	man, err := h.reg.PutManifest(r.Context(), writeRepo, writeImage, ref, mediaType, body)
	if err != nil {
		mapManifestReadError(w, err)
		return
	}
	w.Header().Set("Location", "/v2/"+clientPath+"/manifests/"+ref)
	w.Header().Set("Docker-Content-Digest", man.Digest.String())
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) deleteManifest(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath, ref string) {
	writeRepo, writeImage, err := h.ensureWritable(r.Context(), repo, image, clientPath)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if err := h.reg.DeleteManifest(r.Context(), writeRepo, writeImage, ref); err != nil {
		mapManifestReadError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *handler) listTags(w http.ResponseWriter, r *http.Request, repo model.Repository, image, clientPath string) {
	n := int32(100)
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 32)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, CodeUnsupported, "invalid n parameter")
			return
		}
		n = int32(parsed)
	}
	last := r.URL.Query().Get("last")
	page, err := h.res.ListOCITags(r.Context(), repo.Name, image, last, n)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if page.NextCursor != "" {
		link := `</v2/` + clientPath + `/tags/list?n=` + strconv.FormatInt(int64(n), 10) +
			`&last=` + page.NextCursor + `>; rel="next"`
		w.Header().Set("Link", link)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name": clientPath,
		"tags": page.Items,
	})
}
