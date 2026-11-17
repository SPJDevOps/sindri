// Command preview serves the UI's golden pages with the real stylesheet,
// script, and fonts, so a person can look at them in a browser (SIN-100).
//
// The goldens are what the template tests render, so this shows exactly the
// markup the tests guard, with no database, sign-in, or seeded data. Files
// are read from disk on every request: edit sindri.css or regenerate a
// golden, reload, and the browser shows the change. Run it from the
// repository root with `make ui-preview`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	goldenDir = "internal/ui/testdata"
	staticDir = "web/static"
	// The goldens name the asset version VERSION so they do not change with
	// every stylesheet edit; see TestTemplates_Golden.
	assetPrefix = "/ui/static/VERSION/"
)

var index = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>UI preview · Sindri</title>
<link rel="stylesheet" href="` + assetPrefix + `sindri.css"></head>
<body><main><div class="container">
<h1 class="page-title">UI preview</h1>
<p class="lead">Golden pages from {{.Dir}}, served with the stylesheet from {{.Static}}. Links inside a page lead nowhere.</p>
<div class="rows">{{range .Pages}}<a class="row" href="/p/{{.}}"><span class="primary">{{.}}</span></a>{{end}}</div>
</div></main></body></html>`))

func main() {
	addr := flag.String("addr", "127.0.0.1:8089", "address to listen on")
	flag.Parse()

	for _, dir := range []string{goldenDir, staticDir} {
		if _, err := os.Stat(dir); err != nil {
			fmt.Fprintf(os.Stderr, "preview: %s not found; run from the repository root\n", dir)
			os.Exit(1)
		}
	}

	mux := http.NewServeMux()
	mux.Handle(assetPrefix, http.StripPrefix(assetPrefix, noStore(http.FileServerFS(os.DirFS(staticDir)))))
	mux.HandleFunc("GET /p/{name}", servePage)
	mux.HandleFunc("GET /{$}", serveIndex)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("serving UI preview", "url", "http://"+*addr+"/")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("preview stopped", "error", err)
		os.Exit(1)
	}
}

// noStore keeps the browser from caching assets, so a reload always shows
// the stylesheet as it is on disk now.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, _ *http.Request) {
	pages, err := goldens()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = index.Execute(w, struct {
		Dir, Static string
		Pages       []string
	}{goldenDir, staticDir, pages})
}

func servePage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != path.Base(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(os.DirFS(goldenDir), name+".golden.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The bytes are a checked-in golden page, not request data: the request
	// only picks which file, and the name check above keeps it inside
	// goldenDir. This is a local development tool bound to loopback.
	_, _ = w.Write(b) // #nosec G705 -- serves a checked-in golden file, see above
}

func goldens() ([]string, error) {
	matches, err := fs.Glob(os.DirFS(goldenDir), "*.golden.html")
	if err != nil {
		return nil, fmt.Errorf("listing goldens: %w", err)
	}
	pages := make([]string, 0, len(matches))
	for _, m := range matches {
		pages = append(pages, strings.TrimSuffix(m, ".golden.html"))
	}
	sort.Strings(pages)
	return pages, nil
}
