// Package pypi is the PEP 503/691 hosted index and twine upload adapter
// mounted at /pypi.
//
// Handlers translate the wire protocol into registry calls. Placement is the
// files row from registry.Publish (ADR 12); invariant I2 is enforced on every
// download.
package pypi

import (
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/registry"
)

// Options wires the PyPI adapter.
type Options struct {
	Registry *registry.Registry
}

// Handler returns the /pypi handler. Mount it with r.Mount("/pypi", ...).
//
// A plain handler is used instead of chi path params because repository names
// may contain slashes mid-path (e.g. team/internal/simple/...).
func Handler(opts Options) http.Handler {
	if opts.Registry == nil {
		panic("pypi.Handler: nil registry")
	}
	h := &handler{reg: opts.Registry}
	return http.HandlerFunc(h.serve)
}

type handler struct {
	reg *registry.Registry
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/pypi")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		writeError(w, http.StatusNotFound, "repository path required")
		return
	}
	h.dispatch(w, r, path)
}

func (h *handler) dispatch(w http.ResponseWriter, r *http.Request, path string) {
	repo, rest, kind, ok := splitPyPIPath(path)
	if !ok {
		writeError(w, http.StatusNotFound, "no route matches this path")
		return
	}

	switch kind {
	case pathSimple:
		h.handleSimple(w, r, repo, rest)
	case pathUpload:
		if rest != "" {
			writeError(w, http.StatusNotFound, "no route matches this path")
			return
		}
		h.handleUpload(w, r, repo)
	case pathPackages:
		h.handleDownload(w, r, repo, rest)
	default:
		writeError(w, http.StatusNotFound, "no route matches this path")
	}
}

type pathKind int

const (
	pathSimple pathKind = iota
	pathUpload
	pathPackages
)

// splitPyPIPath splits "{repo}/(simple|upload|packages)/..." where repo may
// contain slashes.
func splitPyPIPath(path string) (repo, rest string, kind pathKind, ok bool) {
	if i := strings.Index(path, "/packages/"); i > 0 {
		return path[:i], strings.Trim(path[i+len("/packages/"):], "/"), pathPackages, true
	}
	if i := strings.Index(path, "/simple/"); i > 0 {
		return path[:i], strings.Trim(path[i+len("/simple/"):], "/"), pathSimple, true
	}
	if i := strings.LastIndex(path, "/simple"); i > 0 {
		suffix := path[i:]
		if suffix == "/simple" || suffix == "/simple/" {
			return path[:i], "", pathSimple, true
		}
	}
	if i := strings.LastIndex(path, "/upload"); i > 0 {
		suffix := path[i:]
		if suffix == "/upload" || suffix == "/upload/" {
			return path[:i], "", pathUpload, true
		}
	}
	return "", "", 0, false
}
