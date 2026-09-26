package pypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/registry"
)

const maxUploadBytes = 512 << 20 // 512 MiB

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
	// MaxBytesReader above caps the body; the memory argument only bounds
	// how much of a large upload is held in RAM vs spilled to disk.
	if err := r.ParseMultipartForm(32 << 20); err != nil { //nolint:gosec // G120: MaxBytesReader bounds the body above
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

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

	file, header, err := r.FormFile("content")
	if err != nil {
		writeError(w, http.StatusBadRequest, "content file part is required")
		return
	}
	defer func() { _ = file.Close() }()

	filename := header.Filename
	if filename == "" {
		writeError(w, http.StatusBadRequest, "content filename is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading upload content")
		return
	}
	if int64(len(body)) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}

	sum := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(sum[:])
	if claimed := strings.TrimSpace(r.FormValue("sha256_digest")); claimed != "" {
		claimed = strings.ToLower(claimed)
		if claimed != hexDigest {
			writeError(w, http.StatusBadRequest, "sha256_digest does not match uploaded bytes")
			return
		}
	}

	meta, err := extractMetadata(filename, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "extracting package metadata: "+err.Error())
		return
	}
	if meta.Name == "" {
		meta.Name = name
	}
	if meta.Version == "" {
		meta.Version = version
	}

	_, err = h.reg.Publish(r.Context(), registry.PublishParams{
		Repo:      repo,
		Package:   name,
		Version:   version,
		Filename:  filename,
		MediaType: mediaTypeForFilename(filename),
		Metadata:  meta.raw(),
		Body:      bytes.NewReader(body),
		Size:      int64(len(body)),
	})
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}
