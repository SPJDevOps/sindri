// Package ui is the read-only web interface under /ui (ADR 21).
//
// Every page is rendered on the server from the same services the management
// API uses, with html/template and a little htmx for "load more". Nothing here
// changes state: auth.GuardUI admits only GET and HEAD, and there are no
// forms.
//
// Authorization is the API's, page by page. A repository the caller may not
// read does not exist as far as the UI is concerned: it is missing from the
// list and its pages are the same 404 as a name that was never created. An
// artifact page is scoped to a repository and checks placement before it
// shows anything, because deduplication makes every tenant's digests
// physically present for every other tenant (invariant I2).
package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/version"
	"github.com/spj/sindri/web"
)

// Prefix is where the server mounts the UI.
const Prefix = "/ui"

// pageSize is how many rows a page or a "load more" adds.
const pageSize = 50

// contentSecurityPolicy allows nothing but this server's own files: no inline
// script or style, no third-party origin, no forms, no framing. The templates
// are tested to contain nothing that would need more.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Options wires the UI to the services it reads.
type Options struct {
	Registry  *registry.Registry
	Lifecycle *lifecycle.Service
	Evidence  *evidence.Service
	Logger    *slog.Logger
	// RequestID extracts the request id the server middleware set, so an
	// error page can name the log line.
	RequestID func(context.Context) string
	// SignOut shows a sign-out control, which needs browser sessions
	// (auth.oidc) to be configured.
	SignOut bool
}

type handler struct {
	opts   Options
	pages  map[string]*template.Template
	static http.Handler
	router chi.Router
}

// Handler returns the UI router, to be wrapped in auth.GuardUI and mounted at
// Prefix.
func Handler(opts Options) (http.Handler, error) {
	h, err := newHandler(opts)
	if err != nil {
		return nil, err
	}
	return h.router, nil
}

func newHandler(opts Options) (*handler, error) {
	if opts.Registry == nil || opts.Lifecycle == nil || opts.Evidence == nil {
		return nil, errors.New("ui: registry, lifecycle, and evidence are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.RequestID == nil {
		opts.RequestID = func(context.Context) string { return "" }
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	h := &handler{
		opts:   opts,
		pages:  pages,
		static: http.StripPrefix(Prefix+"/static/"+web.Version+"/", http.FileServerFS(web.Static())),
	}

	r := chi.NewRouter()
	r.Use(middleware.GetHead, securityHeaders)
	r.Get("/", h.repositories)
	r.Get("/r/*", h.repository)
	r.Get("/static/{version}/*", h.asset)
	r.NotFound(h.notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the UI is read-only", http.StatusMethodNotAllowed)
	})
	h.router = r
	return h, nil
}

var pageFiles = []string{"repos", "repo", "package", "artifact", "notfound", "error"}

func parsePages() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(pageFiles))
	for _, name := range pageFiles {
		t, err := template.New(name).Funcs(funcs).ParseFS(web.Templates(), "layout.html", name+".html")
		if err != nil {
			return nil, fmt.Errorf("ui: parsing template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

var funcs = template.FuncMap{
	"asset": func(name string) string { return Prefix + "/static/" + web.Version + "/" + name },
	"short": shortDigest,
	"bytes": humanBytes,
	"ago":   ago,
	"when":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}

// securityHeaders goes on every response, pages and assets alike.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", contentSecurityPolicy)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// asset serves an embedded file under the current version. The version in the
// path is what makes caching it forever safe; an old one is a 404, not stale
// bytes.
func (h *handler) asset(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "version") != web.Version {
		http.NotFound(w, r)
		return
	}
	if _, err := fs.Stat(web.Static(), chi.URLParam(r, "*")); err != nil {
		http.NotFound(w, r)
		return
	}
	// private: the assets are public source, but they sit behind sign-in,
	// and a shared cache has no business holding responses to a credential.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	h.static.ServeHTTP(w, r)
}

// chrome is what every page's layout needs.
type chrome struct {
	Title   string
	Who     string
	SignOut bool
	Version string
	Crumbs  []crumb
}

type crumb struct {
	Label string
	Href  string
}

func (h *handler) chrome(r *http.Request, title string, crumbs ...crumb) chrome {
	p := principal(r)
	return chrome{
		Title:   title,
		Who:     p.Subject,
		SignOut: h.opts.SignOut && p.Kind == auth.KindUser,
		Version: version.Version,
		Crumbs:  crumbs,
	}
}

// render executes a whole page, or only the named partial when htmx asked
// for one. It renders into a buffer first, so a template error becomes an
// error page rather than half a page with a 200.
func (h *handler) render(w http.ResponseWriter, r *http.Request, page, partial string, status int, data any) {
	t := h.pages[page]
	name := "layout"
	if partial != "" && r.Header.Get("HX-Request") == "true" {
		name = partial
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		h.fail(w, r, fmt.Errorf("rendering %s: %w", page, err))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	// Every page depends on who is asking.
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Vary", "Cookie, Authorization, HX-Request")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "notfound", "", http.StatusNotFound, h.chrome(r, "Not found"))
}

// fail logs err and answers with an error page that names the request, never
// the error: the cause belongs in the log, not in front of the caller.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	id := h.opts.RequestID(r.Context())
	h.opts.Logger.ErrorContext(r.Context(), "ui request failed", "path", r.URL.Path, "request_id", id, "error", err.Error())
	data := struct {
		chrome
		RequestID string
	}{h.chrome(r, "Error"), id}
	var buf bytes.Buffer
	if h.pages["error"].ExecuteTemplate(&buf, "layout", data) != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(buf.Bytes())
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.PrincipalFromContext(r.Context())
	return p
}

// readable reports whether the caller may read repo. auth.Authorize is the
// one authorization function; the UI never decides on its own.
func readable(r *http.Request, repo model.Repository) bool {
	return auth.Authorize(principal(r), repo, auth.RoleRead) == nil
}

// requireRepo loads a repository the caller may read, or answers 404 for one
// that does not exist and for one the caller may not read alike, so the UI
// never confirms a name to someone who cannot see it.
func (h *handler) requireRepo(w http.ResponseWriter, r *http.Request, name string) (model.Repository, bool) {
	repo, err := h.opts.Registry.GetRepository(r.Context(), name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			h.notFound(w, r)
			return model.Repository{}, false
		}
		h.fail(w, r, err)
		return model.Repository{}, false
	}
	if !readable(r, repo) {
		h.notFound(w, r)
		return model.Repository{}, false
	}
	return repo, true
}

// repository dispatches /ui/r/{name}[/-/{page}]. Repository names contain
// slashes, so the page within one follows a /-/ separator, which the
// repository name rule reserves (registry.ValidateRepoName).
func (h *handler) repository(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(chi.URLParam(r, "*"), "/")
	name, page, hasPage := strings.Cut(path, "/-/")
	if name == "" {
		h.notFound(w, r)
		return
	}
	repo, ok := h.requireRepo(w, r, name)
	if !ok {
		return
	}
	switch {
	case !hasPage:
		h.repositoryPage(w, r, repo)
	case page == "package":
		h.packagePage(w, r, repo)
	case page == "artifact":
		h.artifactPage(w, r, repo)
	default:
		h.notFound(w, r)
	}
}

// Links. Every href is built here, so the templates never assemble a URL.

func repoHref(name string) string { return Prefix + "/r/" + name }

func packageHref(repo, pkg string) string {
	return repoHref(repo) + "/-/package?" + url.Values{"name": {pkg}}.Encode()
}

func artifactHref(repo, image string, d model.Digest) string {
	q := url.Values{"digest": {d.String()}}
	if image != "" {
		q.Set("image", image)
	}
	return repoHref(repo) + "/-/artifact?" + q.Encode()
}

func shortDigest(d any) string {
	s := fmt.Sprint(d)
	algo, hex, ok := strings.Cut(s, ":")
	if !ok || len(hex) <= 12 {
		return s
	}
	return algo + ":" + hex[:12]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	case d < 60*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	default:
		return t.UTC().Format("2 Jan 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
