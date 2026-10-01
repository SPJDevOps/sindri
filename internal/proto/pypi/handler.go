// Package pypi is the PEP 503/691 hosted index and twine upload adapter
// mounted at /pypi.
//
// Handlers translate the wire protocol into resolve/registry calls. Placement
// is the files row from registry.Publish (ADR 12); invariant I2 is enforced
// on every download. Proxy and virtual reads go through resolve (M5).
package pypi

import (
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
)

// Options wires the PyPI adapter.
type Options struct {
	Resolver *resolve.Resolver
}

// Handler returns the /pypi handler. Mount it with r.Mount("/pypi", ...).
//
// A plain handler is used instead of chi path params because repository names
// may contain slashes mid-path (e.g. team/internal/simple/...).
func Handler(opts Options) http.Handler {
	if opts.Resolver == nil {
		panic("pypi.Handler: nil resolver")
	}
	h := &handler{res: opts.Resolver, reg: opts.Resolver.Registry()}
	return http.HandlerFunc(h.serve)
}

type handler struct {
	res *resolve.Resolver
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
//
// The leftmost marker segment wins. That is only unambiguous because
// registry.validateRepoName rejects these words as repository name segments:
// otherwise /pypi/team/packages/simple/foo/ could equally mean the simple
// index of "team/packages" or a download from "team", and a project legitimately
// named "packages" would collide with the download route.
func splitPyPIPath(path string) (repo, rest string, kind pathKind, ok bool) {
	segments := strings.Split(path, "/")
	for i := 1; i < len(segments); i++ {
		kind, isMarker := pathKindForSegment(segments[i])
		if !isMarker {
			continue
		}
		repo = strings.Join(segments[:i], "/")
		if repo == "" {
			return "", "", 0, false
		}
		rest = strings.Trim(strings.Join(segments[i+1:], "/"), "/")
		return repo, rest, kind, true
	}
	return "", "", 0, false
}

func pathKindForSegment(segment string) (pathKind, bool) {
	switch segment {
	case "simple":
		return pathSimple, true
	case "upload":
		return pathUpload, true
	case "packages":
		return pathPackages, true
	default:
		return 0, false
	}
}
