package pypi

import (
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/spj/sindri/internal/registry"
)

const (
	mediaSimpleJSON = "application/vnd.pypi.simple.v1+json"
	mediaSimpleHTML = "application/vnd.pypi.simple.v1+html"
)

func (h *handler) handleSimple(w http.ResponseWriter, r *http.Request, repo, project string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if _, err := h.reg.RequirePyPIRepository(r.Context(), repo); err != nil {
		mapRegistryError(w, err)
		return
	}

	if project == "" {
		h.simpleRoot(w, r, repo)
		return
	}
	if strings.Contains(project, "/") {
		// One segment names a project. Folding a deeper path into a name
		// would make many URLs resolve to one project page.
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	normalized := NormalizeName(project)
	if normalized == "" {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if project != normalized {
		// Relative path built only from validated repo name and PEP 503
		// normalized project; never from a raw external Location.
		target := "/pypi/" + repo + "/simple/" + normalized + "/"
		if !strings.HasPrefix(target, "/pypi/") || strings.Contains(target, "//") || strings.Contains(target, "://") {
			writeError(w, http.StatusBadRequest, "invalid project name")
			return
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}

	h.simpleProject(w, r, repo, normalized)
}

func (h *handler) simpleRoot(w http.ResponseWriter, r *http.Request, repo string) {
	page, err := h.reg.ListPackages(r.Context(), registry.ListPackagesParams{
		Repo:  repo,
		Limit: 200,
	})
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	wantJSON := prefersJSON(r.Header.Get("Accept"))
	if wantJSON {
		type project struct {
			Name string `json:"name"`
		}
		projects := make([]project, 0, len(page.Items))
		for _, pkg := range page.Items {
			projects = append(projects, project{Name: pkg.NormalizedName})
		}
		writeJSON(w, mediaSimpleJSON, map[string]any{
			"meta":     map[string]any{"api-version": "1.0"},
			"projects": projects,
		})
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><title>Simple Index</title></head><body>\n")
	for _, pkg := range page.Items {
		name := html.EscapeString(pkg.NormalizedName)
		b.WriteString(`<a href="`)
		b.WriteString(name)
		b.WriteString(`/">`)
		b.WriteString(name)
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

func (h *handler) simpleProject(w http.ResponseWriter, r *http.Request, repo, project string) {
	files, err := h.reg.ListPackageFiles(r.Context(), repo, project)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if len(files) == 0 {
		// Empty project pages are 404 so pip does not treat an empty index as success.
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	wantJSON := prefersJSON(r.Header.Get("Accept"))
	if wantJSON {
		type fileEntry struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython *string           `json:"requires-python"`
			Size           *int64            `json:"size,omitempty"`
		}
		entries := make([]fileEntry, 0, len(files))
		for _, f := range files {
			url := packageURL(r, repo, project, f.Filename)
			req := requiresPythonFromMetadata(f.Metadata)
			var reqPtr *string
			if req != "" {
				reqPtr = &req
			}
			size := f.Size
			entries = append(entries, fileEntry{
				Filename:       f.Filename,
				URL:            url,
				Hashes:         map[string]string{"sha256": f.Digest.Hex()},
				RequiresPython: reqPtr,
				Size:           &size,
			})
		}
		writeJSON(w, mediaSimpleJSON, map[string]any{
			"meta":     map[string]any{"api-version": "1.0"},
			"name":     project,
			"files":    entries,
			"versions": uniqueVersions(files),
		})
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><title>Links for ")
	b.WriteString(html.EscapeString(project))
	b.WriteString("</title></head><body>\n<h1>Links for ")
	b.WriteString(html.EscapeString(project))
	b.WriteString("</h1>\n")
	for _, f := range files {
		url := packageURL(r, repo, project, f.Filename)
		href := html.EscapeString(url + "#sha256=" + f.Digest.Hex())
		b.WriteString(`<a href="`)
		b.WriteString(href)
		b.WriteString(`"`)
		if req := requiresPythonFromMetadata(f.Metadata); req != "" {
			b.WriteString(` data-requires-python="`)
			b.WriteString(html.EscapeString(req))
			b.WriteString(`"`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(f.Filename))
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

func uniqueVersions(files []registry.PackageFile) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, f := range files {
		if _, ok := seen[f.Version]; ok {
			continue
		}
		seen[f.Version] = struct{}{}
		out = append(out, f.Version)
	}
	return out
}

func packageURL(r *http.Request, repo, project, filename string) string {
	// Absolute URL so pip can follow links regardless of index path depth.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + "/pypi/" + repo + "/packages/" + project + "/" + filename
}

func prefersJSON(accept string) bool {
	if accept == "" {
		return false
	}
	// Prefer JSON when the client explicitly asks for the PyPI JSON media type
	// ahead of HTML, matching PEP 691 content negotiation.
	parts := strings.Split(accept, ",")
	jsonRank, htmlRank := -1, -1
	for i, part := range parts {
		media := strings.TrimSpace(strings.Split(part, ";")[0])
		switch media {
		case mediaSimpleJSON, "application/json":
			if jsonRank < 0 {
				jsonRank = i
			}
		case mediaSimpleHTML, "text/html", "*/*":
			if htmlRank < 0 && media != "*/*" {
				htmlRank = i
			}
		}
	}
	if jsonRank >= 0 && (htmlRank < 0 || jsonRank < htmlRank) {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, contentType string, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", mediaSimpleHTML+"; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
