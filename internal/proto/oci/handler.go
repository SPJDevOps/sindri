package oci

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

// Options wires the OCI adapter.
type Options struct {
	Registry *registry.Registry
	CAS      *cas.Store
}

// Handler returns the /v2 handler. Mount it with r.Mount("/v2", ...).
//
// A plain handler is used instead of chi path params because OCI repository
// names contain slashes mid-path (e.g. demo/app/blobs/...), which no fixed
// segment pattern can express.
func Handler(opts Options) http.Handler {
	if opts.Registry == nil {
		panic("oci.Handler: nil registry")
	}
	if opts.CAS == nil {
		panic("oci.Handler: nil cas")
	}
	h := &handler{reg: opts.Registry, cas: opts.CAS}
	return http.HandlerFunc(h.serve)
}

type handler struct {
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
	name, rest, ok := splitRepoPath(path)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
		return
	}

	switch {
	case rest == "tags/list":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
			return
		}
		h.listTags(w, r, name)
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		if ref == "" {
			writeError(w, http.StatusBadRequest, CodeManifestInvalid, "manifest reference required")
			return
		}
		h.handleManifest(w, r, name, ref)
	case strings.HasPrefix(rest, "blobs/uploads"):
		h.handleUpload(w, r, name, rest)
	case strings.HasPrefix(rest, "blobs/"):
		digestStr := strings.TrimPrefix(rest, "blobs/")
		h.handleBlob(w, r, name, digestStr)
	default:
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
	}
}

// splitRepoPath splits "{name}/(blobs|manifests|tags)/..." where name may
// contain slashes.
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

func (h *handler) handleBlob(w http.ResponseWriter, r *http.Request, name, digestStr string) {
	digest, err := model.ParseDigest(digestStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeDigestInvalid, "invalid digest")
		return
	}

	switch r.Method {
	case http.MethodHead, http.MethodGet:
		h.getBlob(w, r, name, digest)
	case http.MethodDelete:
		if err := h.reg.UnlinkBlob(r.Context(), name, digest); err != nil {
			mapBlobReadError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
	}
}

func (h *handler) getBlob(w http.ResponseWriter, r *http.Request, name string, digest model.Digest) {
	info, err := h.reg.StatBlob(r.Context(), name, digest)
	if err != nil {
		mapBlobReadError(w, err)
		return
	}

	w.Header().Set("Docker-Content-Digest", digest.String())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		_, rc, err := h.reg.GetBlob(r.Context(), name, digest)
		if err != nil {
			mapBlobReadError(w, err)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
		return
	}

	off, length, ok := parseByteRange(rangeHeader, info.Size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, CodeSizeInvalid, "invalid range")
		return
	}
	_, rc, err := h.reg.GetBlobRange(r.Context(), name, digest, off, length)
	if err != nil {
		mapBlobReadError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	end := off + length - 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(info.Size, 10))
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

func (h *handler) handleUpload(w http.ResponseWriter, r *http.Request, name, rest string) {
	rest = strings.TrimSuffix(rest, "/")
	switch {
	case rest == "blobs/uploads":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
			return
		}
		h.startUpload(w, r, name)
	case strings.HasPrefix(rest, "blobs/uploads/"):
		id := storage.UploadID(strings.TrimPrefix(rest, "blobs/uploads/"))
		if id == "" {
			writeError(w, http.StatusNotFound, CodeBlobUploadUnknown, "blob upload unknown to registry")
			return
		}
		switch r.Method {
		case http.MethodPatch:
			h.patchUpload(w, r, name, id)
		case http.MethodPut:
			h.putUpload(w, r, name, id)
		case http.MethodGet:
			h.getUploadStatus(w, r, name, id)
		case http.MethodDelete:
			h.abortUpload(w, r, name, id)
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

func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := h.requireOCI(r, name); err != nil {
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
		info, err := h.reg.CommitBlobUpload(r.Context(), name, upload, digest)
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		w.Header().Set("Docker-Content-Digest", info.Digest.String())
		w.Header().Set("Location", "/v2/"+name+"/blobs/"+info.Digest.String())
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
		w.Header().Set("Location", uploadLocation(name, upload.ID()))
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
	w.Header().Set("Location", uploadLocation(name, upload.ID()))
	w.Header().Set("Range", inclusiveRangeHeader(0))
	w.Header().Set("Docker-Upload-UUID", string(upload.ID()))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusAccepted)
}

func (h *handler) patchUpload(w http.ResponseWriter, r *http.Request, name string, id storage.UploadID) {
	if _, err := h.requireOCI(r, name); err != nil {
		mapRegistryError(w, err)
		return
	}
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
			w.Header().Set("Location", uploadLocation(name, id))
			w.Header().Set("Docker-Upload-UUID", string(id))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	if _, err := upload.Append(r.Context(), r.Body); err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Location", uploadLocation(name, id))
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

func (h *handler) putUpload(w http.ResponseWriter, r *http.Request, name string, id storage.UploadID) {
	if _, err := h.requireOCI(r, name); err != nil {
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
	info, err := h.reg.CommitBlobUpload(r.Context(), name, upload, digest)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Docker-Content-Digest", info.Digest.String())
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+info.Digest.String())
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) getUploadStatus(w http.ResponseWriter, r *http.Request, name string, id storage.UploadID) {
	if _, err := h.requireOCI(r, name); err != nil {
		mapRegistryError(w, err)
		return
	}
	upload, err := h.cas.ResumeUpload(id)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	w.Header().Set("Range", inclusiveRangeHeader(upload.Size()))
	w.Header().Set("Docker-Upload-UUID", string(id))
	w.Header().Set("Location", uploadLocation(name, id))
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) abortUpload(w http.ResponseWriter, r *http.Request, name string, id storage.UploadID) {
	if _, err := h.requireOCI(r, name); err != nil {
		mapRegistryError(w, err)
		return
	}
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

func (h *handler) requireOCI(r *http.Request, name string) (model.Repository, error) {
	return h.reg.EnsureOCIRepository(r.Context(), name)
}

func (h *handler) handleManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, name, ref)
	case http.MethodPut:
		h.putManifest(w, r, name, ref)
	case http.MethodDelete:
		h.deleteManifest(w, r, name, ref)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "method not allowed")
	}
}

func (h *handler) getManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	man, err := h.reg.GetManifest(r.Context(), name, ref)
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

func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeManifestInvalid, "could not read body")
		return
	}
	mediaType := r.Header.Get("Content-Type")
	man, err := h.reg.PutManifest(r.Context(), name, ref, mediaType, body)
	if err != nil {
		mapManifestReadError(w, err)
		return
	}
	w.Header().Set("Location", "/v2/"+name+"/manifests/"+ref)
	w.Header().Set("Docker-Content-Digest", man.Digest.String())
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) deleteManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	if err := h.reg.DeleteManifest(r.Context(), name, ref); err != nil {
		mapManifestReadError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *handler) listTags(w http.ResponseWriter, r *http.Request, name string) {
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
	page, err := h.reg.ListTags(r.Context(), registry.ListTagsParams{
		Repo:  name,
		After: last,
		Limit: n,
	})
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if page.NextCursor != "" {
		link := `</v2/` + name + `/tags/list?n=` + strconv.FormatInt(int64(n), 10) +
			`&last=` + page.NextCursor + `>; rel="next"`
		w.Header().Set("Link", link)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name": name,
		"tags": page.Items,
	})
}
