package npm

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/remote"
)

func (h *handler) handleTarball(w http.ResponseWriter, r *http.Request, rt route) {
	repo, ok := h.authorizeRead(w, r, rt)
	if !ok {
		return
	}
	if !requestableFilename(repo.Type, rt.pkg, rt.file) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// A placed copy's placement in this repository is found before any bytes (I2).
	file, rc, err := h.res.GetNPMTarball(r.Context(), rt.repo, rt.pkg, rt.file)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", file.MediaType)
	// An uncached proxy file has the length upstream declared, if it declared one.
	if file.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+file.Filename+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are sent, so cutting the connection is the only way to tell
		// npm the file is bad: a clean end would hand it bytes that stopped
		// short, or bytes from upstream that failed their hash. The recoverer
		// passes this panic value through.
		panic(http.ErrAbortHandler)
	}
}

// requestableFilename keeps a filename that could not exist from reaching the
// database, which also keeps a NUL or invalid UTF-8 from the path out of a query.
// A hosted file is always tarballFilename(name, v), but a proxied one is named by
// upstream, and found by matching its dist.tarball.
func requestableFilename(repoType model.RepoType, name, file string) bool {
	if repoType == model.RepoTypeHosted {
		return isTarballFilename(name, file)
	}
	return remote.ValidNPMFilename(file)
}

// isTarballFilename reports whether file is tarballFilename(name, v) for some
// valid version v. Anything else cannot have been published.
func isTarballFilename(name, file string) bool {
	version, ok := strings.CutPrefix(file, name[strings.LastIndex(name, "/")+1:]+"-")
	if !ok {
		return false
	}
	version, ok = strings.CutSuffix(version, ".tgz")
	return ok && validVersion(version)
}
