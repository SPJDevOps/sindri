package npm

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (h *handler) handleTarball(w http.ResponseWriter, r *http.Request, rt route) {
	if !h.authorizeHostedRead(w, r, rt) {
		return
	}
	if !isTarballFilename(rt.pkg, rt.file) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// GetNPMTarball finds the placement in this repository before any bytes (I2).
	file, rc, err := h.reg.GetNPMTarball(r.Context(), rt.repo, rt.pkg, rt.file)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", file.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+file.Filename+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are sent, so cutting the connection is the only way to tell
		// npm the file is bad: a clean end would hand it bytes that stopped
		// short. The recoverer passes this panic value through.
		panic(http.ErrAbortHandler)
	}
}

// isTarballFilename reports whether file is tarballFilename(name, v) for some
// valid version v. Anything else cannot have been published, so it never reaches
// the database, which also keeps a NUL or invalid UTF-8 from the path out of a
// query.
func isTarballFilename(name, file string) bool {
	version, ok := strings.CutPrefix(file, name[strings.LastIndex(name, "/")+1:]+"-")
	if !ok {
		return false
	}
	version, ok = strings.CutSuffix(version, ".tgz")
	return ok && validVersion(version)
}
