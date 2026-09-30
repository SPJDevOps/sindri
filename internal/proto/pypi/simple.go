package pypi

import (
	"encoding/json"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/spj/sindri/internal/resolve"
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
	names, err := h.res.ListPyPIProjects(r.Context(), repo)
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

func (h *handler) simpleProject(w http.ResponseWriter, r *http.Request, repo, project string) {
	resolved, err := h.res.ListPyPIFiles(r.Context(), repo, project)
	if err != nil {
		mapRegistryError(w, err)
		return
	}

	entries := indexEntries(resolved)
	if len(entries) == 0 {
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
			Size           int64             `json:"size"`
		}
		files := make([]fileEntry, 0, len(entries))
		versions := map[string]struct{}{}
		for _, e := range entries {
			var reqPtr *string
			if e.requires != "" {
				req := e.requires
				reqPtr = &req
			}
			hashes := map[string]string{}
			if e.sha256 != "" {
				hashes["sha256"] = e.sha256
			}
			// size is required at api-version 1.1 (PEP 700). An upstream on
			// an older api-version may not have said, and 0 is the least
			// wrong number; pip does not use it.
			size := e.size
			if size < 0 {
				size = 0
			}
			files = append(files, fileEntry{
				Filename:       e.filename,
				URL:            packageURL(repo, project, e.filename),
				Hashes:         hashes,
				RequiresPython: reqPtr,
				Size:           size,
			})
			versions[e.version] = struct{}{}
		}
		verList := make([]string, 0, len(versions))
		for v := range versions {
			verList = append(verList, v)
		}
		sort.Strings(verList)
		writeJSON(w, mediaSimpleJSON, map[string]any{
			"meta":     map[string]any{"api-version": apiVersionJSON},
			"name":     project,
			"files":    files,
			"versions": verList,
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
	for _, e := range entries {
		href := packageURL(repo, project, e.filename)
		if e.sha256 != "" {
			href += "#sha256=" + e.sha256
		}
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(href))
		b.WriteString(`"`)
		if e.requires != "" {
			b.WriteString(` data-requires-python="`)
			b.WriteString(html.EscapeString(e.requires))
			b.WriteString(`"`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(e.filename))
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

// indexEntry is one file link on a project page, whether its bytes are placed
// here already or only known from a proxy's upstream index.
type indexEntry struct {
	filename string
	sha256   string // hex; empty when an upstream index gave no hash
	requires string
	size     int64 // negative when unknown
	version  string
}

// indexEntries lists every file a project page must link.
//
// For a proxy the upstream index is the authority on what exists, and placed
// files are only the subset fetched so far: rendering placed files alone would
// hide every other version and platform the moment one file is cached. Placed
// files come first and win on a filename collision, because their hash is one
// Sindri computed. A placed file upstream no longer lists stays: the cache
// keeps serving what it served before.
func indexEntries(resolved resolve.PyPIProjectFiles) []indexEntry {
	entries := make([]indexEntry, 0, len(resolved.Files))
	placed := make(map[string]struct{}, len(resolved.Files))
	for _, f := range resolved.Files {
		placed[f.Filename] = struct{}{}
		entries = append(entries, indexEntry{
			filename: f.Filename,
			sha256:   f.Digest.Hex(),
			requires: requiresPythonFromMetadata(f.Metadata),
			size:     f.Size,
			version:  f.Version,
		})
	}
	if resolved.Index == nil {
		return entries
	}
	for _, f := range resolved.Index.Files {
		if _, ok := placed[f.Filename]; ok || f.Yanked {
			continue
		}
		entries = append(entries, indexEntry{
			filename: f.Filename,
			sha256:   f.Hashes["sha256"],
			requires: f.Requires,
			size:     f.Size,
			version:  versionFromPyPIFilename(f.Filename),
		})
	}
	return entries
}

func versionFromPyPIFilename(filename string) string {
	base := strings.TrimSuffix(filename, ".whl")
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".zip")
	parts := strings.Split(base, "-")
	if len(parts) >= 2 {
		return parts[1]
	}
	return "0"
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
