package npm

// The hosted npm adapter. Routes are parsed from the escaped path, since a
// client sends @acme%2fui and the decoded form loses where the repository
// ends. Every route authorizes except the two login routes (ADR 24).

import (
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
)

// Options wires the npm adapter.
type Options struct {
	Resolver *resolve.Resolver
	// Auth authenticates every request except the two login routes.
	Auth auth.Authenticator
	// Verifier checks a token presented at login before the adapter answers
	// with it.
	Verifier auth.TokenVerifier
	// PublicURL is the externally visible base URL, used to write tarball
	// URLs into packuments.
	PublicURL string
}

// Handler returns the /npm handler. Mount it with r.Mount("/npm", ...).
func Handler(opts Options) http.Handler {
	if opts.Resolver == nil {
		panic("npm.Handler: nil resolver")
	}
	if opts.Auth == nil {
		panic("npm.Handler: nil authenticator")
	}
	if opts.Verifier == nil {
		panic("npm.Handler: nil token verifier")
	}
	h := &handler{
		res:       opts.Resolver,
		reg:       opts.Resolver.Registry(),
		verifier:  opts.Verifier,
		publicURL: strings.TrimRight(opts.PublicURL, "/"),
	}
	h.authenticate = auth.Middleware(opts.Auth, denyUnauthenticated)
	return http.HandlerFunc(h.serve)
}

type handler struct {
	res       *resolve.Resolver
	reg       *registry.Registry
	verifier  auth.TokenVerifier
	publicURL string
	// authenticate wraps a handler so it runs only for an authenticated caller.
	authenticate func(http.Handler) http.Handler
}

// serve parses the route, then authenticates. The login routes carry no
// credential yet; every other request, including one no route matches, meets
// the middleware first so an anonymous caller learns nothing about routes.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/npm/")
	rt, ok := parseRoute(path)
	if ok && (rt.kind == routeWebLogin || rt.kind == routeUserLogin) {
		h.serveRoute(w, r, rt)
		return
	}
	h.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			writeError(w, http.StatusNotFound, "no route matches this path")
			return
		}
		h.serveRoute(w, r, rt)
	})).ServeHTTP(w, r)
}

// denyUnauthenticated sends the Bearer challenge npm answers with the token in
// .npmrc.
func denyUnauthenticated(w http.ResponseWriter, _ *http.Request, _ error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="sindri"`)
	writeError(w, http.StatusUnauthorized, "authentication required")
}

// authorize checks the caller against the named repository. A caller who may
// not read it is told it does not exist.
//
//nolint:unused // the routes of tasks 6 to 8 call it
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, name string, need auth.Role) bool {
	repo, err := h.reg.GetRepository(r.Context(), name)
	if err != nil {
		mapRegistryError(w, err)
		return false
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	if err := auth.Authorize(p, repo, need); err != nil {
		if need == auth.RoleWrite && auth.Authorize(p, repo, auth.RoleRead) == nil {
			writeError(w, http.StatusForbidden, "the caller may not publish to "+name)
			return false
		}
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// serveRoute runs one parsed route. Tasks 6 to 8 fill these in.
func (h *handler) serveRoute(w http.ResponseWriter, _ *http.Request, rt route) {
	switch rt.kind {
	case routePackument, routeTarball, routeDistTags, routeWhoami, routeWebLogin, routeUserLogin:
		writeError(w, http.StatusNotImplemented, "not implemented")
	default:
		writeError(w, http.StatusNotFound, "no route matches this path")
	}
}
