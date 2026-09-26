package pypi

import (
	"context"
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

	// apiVersionJSON is 1.1 because the JSON serialization carries PEP 700's
	// additions: versions on a project page and size on every file.
	apiVersionJSON = "1.1"
	// apiVersionHTML is what the PEP 629 meta tag reports. The HTML
	// serialization has nowhere to put PEP 700's fields, so it is honestly a
	// 1.0 document; clients ignore a minor version difference and reject a
	// major one, which is the part that matters.
	apiVersionHTML = "1.0"

	// rootPageLimit is the page size used to walk projects for the root index,
	// not a bound on how many are listed.
	rootPageLimit = 200
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
		// PEP 503 index URLs end in a slash, and the root listing's links are
		// relative to it: served from /simple, "demo-package/" would resolve
		// one path segment too high.
		if !strings.HasSuffix(r.URL.Path, "/") {
			redirectTo(w, r.URL.EscapedPath()+"/")
			return
		}
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
	if project != normalized || !strings.HasSuffix(r.URL.Path, "/") {
		// Path built only from the validated repository name and the PEP 503
		// normalized project; never from a raw external Location.
		redirectTo(w, "/pypi/"+repo+"/simple/"+normalized+"/")
		return
	}

	h.simpleProject(w, r, repo, normalized)
}

// redirectTo sends a permanent redirect to a path this package constructed.
// The guard is a belt on top of braces: every caller builds target from a
// validated repository name and a normalized project, so a scheme or an
// authority appearing here would mean a bug upstream, not a hostile request.
func redirectTo(w http.ResponseWriter, target string) {
	if !strings.HasPrefix(target, "/pypi/") || strings.Contains(target, "//") || strings.Contains(target, "://") {
		writeError(w, http.StatusBadRequest, "invalid index path")
		return
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusMovedPermanently)
}

func (h *handler) simpleRoot(w http.ResponseWriter, r *http.Request, repo string) {
	names, err := h.projectNames(r.Context(), repo)
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	if prefersJSON(r.Header.Get("Accept")) {
		type project struct {
			Name string `json:"name"`
		}
		projects := make([]project, 0, len(names))
		for _, name := range names {
			projects = append(projects, project{Name: name})
		}
		writeJSON(w, mediaSimpleJSON, map[string]any{
			"meta":     map[string]any{"api-version": apiVersionJSON},
			"projects": projects,
		})
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head>\n")
	b.WriteString(`<meta name="pypi:repository-version" content="` + apiVersionHTML + `">` + "\n")
	b.WriteString("<title>Simple Index</title>\n</head><body>\n")
	for _, name := range names {
		escaped := html.EscapeString(name)
		b.WriteString(`<a href="`)
		b.WriteString(escaped)
		b.WriteString(`/">`)
		b.WriteString(escaped)
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

// projectNames walks every page of packages in a repository. The root index
// lists a repository in full: a single page would silently stop at the page
// limit, and pip gives no hint that a project it cannot see exists.
func (h *handler) projectNames(ctx context.Context, repo string) ([]string, error) {
	var (
		names  []string
		cursor string
	)
	for {
		page, err := h.reg.ListPackages(ctx, registry.ListPackagesParams{
			Repo:  repo,
			After: cursor,
			Limit: rootPageLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, pkg := range page.Items {
			names = append(names, pkg.NormalizedName)
		}
		// The cursor is a normalized name, unique per repository, so it
		// strictly advances. Stopping when it does not is cheap insurance
		// against a loop that hangs a request handler.
		if page.NextCursor == "" || page.NextCursor == cursor {
			return names, nil
		}
		cursor = page.NextCursor
	}
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
		// size is required at api-version 1.1 (PEP 700); requires-python is
		// nullable rather than omitted (PEP 691).
		type fileEntry struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython *string           `json:"requires-python"`
			Size           int64             `json:"size"`
		}
		entries := make([]fileEntry, 0, len(files))
		for _, f := range files {
			req := requiresPythonFromMetadata(f.Metadata)
			var reqPtr *string
			if req != "" {
				reqPtr = &req
			}
			entries = append(entries, fileEntry{
				Filename:       f.Filename,
				URL:            packageURL(repo, project, f.Filename),
				Hashes:         map[string]string{"sha256": f.Digest.Hex()},
				RequiresPython: reqPtr,
				Size:           f.Size,
			})
		}
		writeJSON(w, mediaSimpleJSON, map[string]any{
			"meta":     map[string]any{"api-version": apiVersionJSON},
			"name":     project,
			"files":    entries,
			"versions": uniqueVersions(files),
		})
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head>\n")
	b.WriteString(`<meta name="pypi:repository-version" content="` + apiVersionHTML + `">` + "\n")
	b.WriteString("<title>Links for ")
	b.WriteString(html.EscapeString(project))
	b.WriteString("</title>\n</head><body>\n<h1>Links for ")
	b.WriteString(html.EscapeString(project))
	b.WriteString("</h1>\n")
	for _, f := range files {
		url := packageURL(repo, project, f.Filename)
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

// packageURL is the download path for a file, relative to the server root.
//
// It is deliberately not absolute. An absolute URL has to come from somewhere,
// and the only sources available per request are the Host header and
// X-Forwarded-Proto, which are client input: reflecting them puts a value a
// requester chose into the index it is served. A root-relative path resolves
// against whatever origin the client actually reached, so TLS termination, a
// port mapping, and a rename all work without configuration. Serving files
// from a different origin than the index (a CDN) would need a configured
// external base URL, and can have one when something asks for it.
func packageURL(repo, project, filename string) string {
	return "/pypi/" + repo + "/packages/" + project + "/" + filename
}

// prefersJSON implements PEP 691 content negotiation over the Accept header.
//
// Quality values decide, not position: pip sends the JSON type at an implicit
// q=1 and the HTML types at q=0.2 and q=0.01, but a client that spells its
// preference the other way round has to be honoured too. Position only breaks
// a tie. A client that asks for neither (a browser sending */*) gets HTML,
// which is the serialization a human can read.
func prefersJSON(accept string) bool {
	jsonQ, htmlQ := -1.0, -1.0
	jsonPos, htmlPos := 0, 0

	for i, part := range strings.Split(accept, ",") {
		media, q, ok := parseAcceptPart(part)
		if !ok || q <= 0 {
			// q=0 is "not acceptable", so it does not count as asking.
			continue
		}
		switch media {
		case mediaSimpleJSON, "application/json":
			if q > jsonQ {
				jsonQ, jsonPos = q, i
			}
		case mediaSimpleHTML, "text/html":
			if q > htmlQ {
				htmlQ, htmlPos = q, i
			}
		}
	}

	switch {
	case jsonQ < 0:
		return false
	case htmlQ < 0:
		return true
	case jsonQ != htmlQ:
		return jsonQ > htmlQ
	default:
		return jsonPos < htmlPos
	}
}

// parseAcceptPart splits one Accept entry into its media type and q value. A
// missing or malformed q means 1, per RFC 9110.
func parseAcceptPart(part string) (media string, q float64, ok bool) {
	fields := strings.Split(part, ";")
	media = strings.ToLower(strings.TrimSpace(fields[0]))
	if media == "" {
		return "", 0, false
	}
	q = 1
	for _, field := range fields[1:] {
		name, value, found := strings.Cut(field, "=")
		if !found || strings.ToLower(strings.TrimSpace(name)) != "q" {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		q = parsed
	}
	return media, q, true
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
