package ui

import (
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/model"
)

// tabLink is one client in the panel's tab row. The current client lives in
// the URL as ?client=, so the choice survives a reload and a shared link, and
// the tabs are plain links.
type tabLink struct {
	Label   string
	Href    string
	Current bool
}

// usePanel is the "Use this" section: commands for the reader to paste.
type usePanel struct {
	Title string
	Tabs  []tabLink
	Steps []cmdStep
	Notes []string
	// Fallback is set when publicUrl is not configured and the commands use
	// the address the page was loaded from.
	Fallback bool
}

// origin is the address clients should use for this server. publicUrl, the
// operator's statement, wins. Without it the page falls back to the request's
// own scheme and host, and does not read X-Forwarded-*: the server ignores
// those headers everywhere, because they are client input, and a command
// pasted into a shell is the wrong place to start trusting them.
func (h *handler) origin(r *http.Request) (base string, fallback bool) {
	if p := strings.TrimRight(h.opts.PublicURL, "/"); p != "" {
		return p, false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host, true
}

// commandInput starts a cmdInput for repo from the request: where the server
// is, and which client was asked for.
func (h *handler) commandInput(r *http.Request, repo model.Repository) cmdInput {
	base, fallback := h.origin(r)
	return cmdInput{
		Base:     base,
		Repo:     repo.Name,
		Protocol: repo.Protocol,
		Kind:     repo.Type,
		Client:   pickClient(repo.Protocol, r.URL.Query().Get("client")),
		Fallback: fallback,
	}
}

// panel assembles the section, or nothing when there are no commands.
func (h *handler) panel(r *http.Request, title string, in cmdInput, steps []cmdStep, notes []string) *usePanel {
	if len(steps) == 0 {
		return nil
	}
	p := &usePanel{Title: title, Steps: steps, Notes: notes, Fallback: in.Fallback}
	for _, c := range clientsFor(in.Protocol) {
		q := r.URL.Query()
		q.Set("client", c)
		p.Tabs = append(p.Tabs, tabLink{Label: c, Href: r.URL.EscapedPath() + "?" + q.Encode(), Current: c == in.Client})
	}
	return p
}

// docsPage joins a documentation page onto ui.docsUrl, or returns "" when
// the server has none.
func (h *handler) docsPage(page string) string {
	if h.opts.DocsURL == "" {
		return ""
	}
	return strings.TrimRight(h.opts.DocsURL, "/") + "/" + page
}
