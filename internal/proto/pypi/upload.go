package pypi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

const (
	maxUploadBytes = 512 << 20 // 512 MiB
	// multipartMemory is how much of an upload is held in RAM before the
	// runtime spills the part to a temporary file. It is not an upload size
	// limit: the file is streamed into the CAS from wherever it landed.
	multipartMemory = 32 << 20 // 32 MiB
)

func (h *handler) handleUpload(w http.ResponseWriter, r *http.Request, repo string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Twine always sends Basic credentials. M4 accepts any Basic header
	// without validating identity (ADR 12); M7 wires real auth.
	user, _, ok := r.BasicAuth()
	if !ok || user == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="sindri"`)
		writeError(w, http.StatusUnauthorized, "basic authentication required")
		return
	}

	if _, err := h.reg.RequirePyPIRepository(r.Context(), repo); err != nil {
		mapRegistryError(w, err)
		return
	}

	// Bound the whole request body before multipart parsing so a client
	// cannot exhaust memory with endless form fields (gosec G120).
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(multipartMemory); err != nil { //nolint:gosec // G120: MaxBytesReader bounds the body above
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	action := r.FormValue(":action")
	if action != "" && action != "file_upload" {
		writeError(w, http.StatusBadRequest, "unsupported :action")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	if name == "" || version == "" {
		writeError(w, http.StatusBadRequest, "name and version are required")
		return
	}
	project := NormalizeName(name)
	if project == "" {
		writeError(w, http.StatusBadRequest, "name does not normalize to a project name")
		return
	}

	file, header, err := r.FormFile("content")
	if err != nil {
		writeError(w, http.StatusBadRequest, "content file part is required")
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}

	filename := header.Filename
	if err := validateFilename(filename); err != nil {
		writeError(w, http.StatusBadRequest, "invalid filename: "+err.Error())
		return
	}

	// Hash what we actually received. The client's sha256_digest is a claim
	// checked against these bytes and never the key anything is stored under
	// (security rule 2); the digest handed to the CAS below is this one.
	digest, err := hashUpload(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading upload content")
		return
	}
	if claimed := strings.TrimSpace(r.FormValue("sha256_digest")); claimed != "" {
		if !strings.EqualFold(claimed, digest.Hex()) {
			writeError(w, http.StatusBadRequest, "sha256_digest does not match uploaded bytes")
			return
		}
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "rewinding upload")
		return
	}
	meta, err := extractMetadata(filename, file, header.Size)
	if err != nil {
		writeError(w, http.StatusBadRequest, "extracting package metadata: "+err.Error())
		return
	}

	// The artifact describes itself; the form fields only say where the client
	// thinks it goes. Disagreement is rejected rather than silently resolved,
	// because pip reads the project and version off the filename and would
	// then disagree with the index (CLAUDE.md: validate at write time).
	if err := checkDistAgrees(filename, project, version, meta); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if meta.Name == "" {
		meta.Name = name
	}
	if meta.Version == "" {
		meta.Version = version
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "rewinding upload")
		return
	}
	_, err = h.reg.Publish(r.Context(), registry.PublishParams{
		Repo:         repo,
		Package:      name,
		Version:      meta.Version,
		Filename:     filename,
		MediaType:    mediaTypeForFilename(filename),
		Metadata:     meta.raw(),
		Body:         file,
		Size:         header.Size,
		ExpectDigest: digest,
		Immutable:    true,
	})
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// hashUpload digests the part without holding it in memory. The part is a
// local file or a bounded in-memory buffer by this point, so reading it twice
// costs a local read, not a second copy of the upload on the heap.
func hashUpload(file io.Reader) (model.Digest, error) {
	d := model.NewDigester(model.AlgorithmSHA256)
	if _, err := io.Copy(d, file); err != nil {
		return model.Digest{}, fmt.Errorf("hashing upload: %w", err)
	}
	return d.Digest(), nil
}

// checkDistAgrees rejects an upload whose filename or embedded metadata names a
// different project or version than the one it is being published as.
func checkDistAgrees(filename, project, version string, meta fileMetadata) error {
	dist, err := parseDistFilename(filename)
	if err != nil {
		return fmt.Errorf("invalid filename: %w", err)
	}
	if got := NormalizeName(dist.Project); got != project {
		return fmt.Errorf("filename names project %q, uploaded as %q", got, project)
	}
	if !sameVersion(dist.Version, version) {
		return fmt.Errorf("filename names version %q, uploaded as %q", dist.Version, version)
	}
	if meta.Name != "" {
		if got := NormalizeName(meta.Name); got != project {
			return fmt.Errorf("package metadata names project %q, uploaded as %q", got, project)
		}
	}
	if meta.Version != "" && !sameVersion(meta.Version, version) {
		return fmt.Errorf("package metadata names version %q, uploaded as %q", meta.Version, version)
	}
	return nil
}
