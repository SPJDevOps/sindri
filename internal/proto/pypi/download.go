package pypi

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (h *handler) handleDownload(w http.ResponseWriter, r *http.Request, repo, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	project, filename, ok := strings.Cut(rest, "/")
	if !ok || project == "" || filename == "" || strings.Contains(filename, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	normalized := NormalizeName(project)
	if normalized == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	file, rc, err := h.res.GetPyPIFile(r.Context(), repo, normalized, filename)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()

	if file.MediaType != "" {
		w.Header().Set("Content-Type", file.MediaType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// A proxied file streaming from an upstream that sent no length has none
	// to declare yet.
	if file.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are sent, so cutting the connection is the only way to
		// tell pip the file is bad: a clean end would hand it bytes that
		// failed verification or stopped short. net/http treats this panic
		// value as "abort the response" and the recoverer passes it through.
		panic(http.ErrAbortHandler)
	}
}
