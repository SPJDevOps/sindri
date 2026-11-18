package ui

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The mechanical half of the accessibility pass (SIN-99): rules a machine can
// check on every rendered golden page. What a screen reader sounds like and
// how a page feels at 200% zoom stay a human checklist in web/DESIGN.md.
//
// The goldens are rendered by Go templates, so their markup is well formed. A
// small tolerant tree builder is enough, and it keeps golang.org/x/net/html
// out of go.mod.

// node is one element of a golden page, or a run of text when tag is empty.
type node struct {
	tag      string
	attrs    map[string]string
	children []*node
	text     string
}

var (
	tagPattern  = regexp.MustCompile(`(?s)<!--.*?-->|<(/?)([a-zA-Z][a-zA-Z0-9-]*)((?:"[^"]*"|'[^']*'|[^'">])*)>`)
	attrPattern = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	voidTags    = map[string]bool{"meta": true, "link": true, "img": true, "input": true, "br": true, "hr": true}
)

func parseMarkup(src string) *node {
	root := &node{tag: "#root"}
	stack := []*node{root}
	top := func() *node { return stack[len(stack)-1] }
	pos := 0
	for _, m := range tagPattern.FindAllStringSubmatchIndex(src, -1) {
		if m[0] > pos {
			top().children = append(top().children, &node{text: html.UnescapeString(src[pos:m[0]])})
		}
		pos = m[1]
		if m[2] < 0 { // a comment
			continue
		}
		closing := src[m[2]:m[3]] == "/"
		tag := strings.ToLower(src[m[4]:m[5]])
		if closing {
			for i := len(stack) - 1; i > 0; i-- {
				if stack[i].tag == tag {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		n := &node{tag: tag, attrs: map[string]string{}}
		for _, a := range attrPattern.FindAllStringSubmatch(src[m[6]:m[7]], -1) {
			v := a[2] + a[3] + a[4]
			n.attrs[strings.ToLower(a[1])] = html.UnescapeString(v)
		}
		top().children = append(top().children, n)
		if !voidTags[tag] && !strings.HasSuffix(strings.TrimSpace(src[m[6]:m[7]]), "/") {
			stack = append(stack, n)
		}
	}
	return root
}

func (n *node) walk(fn func(*node)) {
	fn(n)
	for _, c := range n.children {
		c.walk(fn)
	}
}

func (n *node) find(tag string) []*node {
	var out []*node
	n.walk(func(x *node) {
		if x.tag == tag {
			out = append(out, x)
		}
	})
	return out
}

func (n *node) has(attr string) bool { _, ok := n.attrs[attr]; return ok }

// name is the text a screen reader would use for n's content: text and image
// alt text, skipping anything aria-hidden.
func (n *node) content() string {
	if n.tag == "" {
		return n.text
	}
	if n.attrs["aria-hidden"] == "true" {
		return ""
	}
	if n.tag == "img" {
		return n.attrs["alt"]
	}
	var b strings.Builder
	for _, c := range n.children {
		b.WriteString(c.content())
	}
	return b.String()
}

// accessibleName follows the order of the accessible name computation that
// matters here: aria-labelledby, aria-label, content, then title.
func (n *node) accessibleName(ids map[string]*node) string {
	if refs := strings.Fields(n.attrs["aria-labelledby"]); len(refs) > 0 {
		var parts []string
		for _, r := range refs {
			if t, ok := ids[r]; ok {
				parts = append(parts, t.content())
			}
		}
		if s := strings.TrimSpace(strings.Join(parts, " ")); s != "" {
			return s
		}
	}
	if s := strings.TrimSpace(n.attrs["aria-label"]); s != "" {
		return s
	}
	if s := strings.TrimSpace(n.content()); s != "" {
		return s
	}
	return strings.TrimSpace(n.attrs["title"])
}

// goldenPage is one parsed file under testdata. A full page starts with a
// doctype; the header goldens are fragments and only get the rules that make
// sense without a document around them.
type goldenPage struct {
	name string
	full bool
	root *node
	ids  map[string]*node
	dups []string
}

func goldenPages(t *testing.T) []goldenPage {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden.html"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden pages under testdata: %v", err)
	}
	var pages []goldenPage
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: a glob under testdata
		if err != nil {
			t.Fatal(err)
		}
		p := goldenPage{
			name: strings.TrimSuffix(filepath.Base(f), ".golden.html"),
			full: strings.HasPrefix(string(b), "<!doctype html>"),
			root: parseMarkup(string(b)),
			ids:  map[string]*node{},
		}
		p.root.walk(func(n *node) {
			if id := n.attrs["id"]; id != "" {
				if _, seen := p.ids[id]; seen {
					p.dups = append(p.dups, id)
				}
				p.ids[id] = n
			}
		})
		pages = append(pages, p)
	}
	return pages
}

// eachPage runs check on every golden page, full only or all, as a named
// subtest so a failure says which page broke which rule.
func eachPage(t *testing.T, fullOnly bool, check func(t *testing.T, p goldenPage)) {
	t.Helper()
	for _, p := range goldenPages(t) {
		if fullOnly && !p.full {
			continue
		}
		t.Run(p.name, func(t *testing.T) { check(t, p) })
	}
}

func TestGoldens_EveryPageHasOneH1(t *testing.T) {
	eachPage(t, true, func(t *testing.T, p goldenPage) {
		if got := len(p.root.find("h1")); got != 1 {
			t.Errorf("%d h1 elements, want exactly one", got)
		}
	})
}

func TestGoldens_HeadingsNeverSkipALevelDownward(t *testing.T) {
	eachPage(t, true, func(t *testing.T, p goldenPage) {
		prev := 0
		p.root.walk(func(n *node) {
			if len(n.tag) != 2 || n.tag[0] != 'h' || n.tag[1] < '1' || n.tag[1] > '6' {
				return
			}
			level, _ := strconv.Atoi(n.tag[1:])
			if prev == 0 && level != 1 {
				t.Errorf("first heading is %s %q, want h1", n.tag, strings.TrimSpace(n.content()))
			}
			if prev != 0 && level > prev+1 {
				t.Errorf("%s %q follows h%d: skips a level", n.tag, strings.TrimSpace(n.content()), prev)
			}
			if strings.TrimSpace(n.content()) == "" {
				t.Errorf("%s is empty", n.tag)
			}
			prev = level
		})
	})
}

func TestGoldens_LandmarksAndDocumentBasics(t *testing.T) {
	eachPage(t, true, func(t *testing.T, p goldenPage) {
		for _, tag := range []string{"main", "header", "footer"} {
			if got := len(p.root.find(tag)); got != 1 {
				t.Errorf("%d <%s> landmarks, want exactly one", got, tag)
			}
		}
		if mains := p.root.find("main"); len(mains) == 1 && mains[0].attrs["id"] != "main" {
			t.Error(`<main> lacks id="main", the target of the skip link`)
		}
		if skip := p.root.find("a"); len(skip) == 0 || skip[0].attrs["href"] != "#main" {
			t.Error("the first link is not the skip link to #main")
		}
		navs := p.root.find("nav")
		if len(navs) == 0 {
			t.Error("no <nav> for primary navigation")
		}
		for _, n := range navs {
			if n.attrs["aria-label"] == "" {
				t.Error("a <nav> has no aria-label, so several navs cannot be told apart")
			}
		}
		html := p.root.find("html")
		if len(html) != 1 || html[0].attrs["lang"] == "" {
			t.Error("<html> has no lang")
		}
		if titles := p.root.find("title"); len(titles) != 1 || strings.TrimSpace(titles[0].content()) == "" {
			t.Error("the page has no <title>")
		}
		if live, ok := p.ids["live"]; !ok || live.attrs["aria-live"] != "polite" {
			t.Error(`no polite live region with id="live" for announcements`)
		}
	})
}

func TestGoldens_BreadcrumbsAreANavWithAListAndOneCurrentItem(t *testing.T) {
	eachPage(t, true, func(t *testing.T, p goldenPage) {
		for _, n := range p.root.find("nav") {
			if n.attrs["aria-label"] != "Breadcrumb" {
				continue
			}
			if len(n.find("ol")) != 1 {
				t.Error("breadcrumbs are not one <ol>")
			}
			current := 0
			n.walk(func(x *node) {
				if x.attrs["aria-current"] == "page" {
					current++
				}
			})
			if current != 1 {
				t.Errorf("%d breadcrumb items are aria-current=page, want one", current)
			}
		}
	})
}

func TestGoldens_ButtonsLinksAndControlsHaveANameAndImagesHaveAlt(t *testing.T) {
	eachPage(t, false, func(t *testing.T, p goldenPage) {
		p.root.walk(func(n *node) {
			switch n.tag {
			case "button", "summary", "select", "textarea":
			case "a":
				if !n.has("href") {
					return
				}
			case "input":
				if n.attrs["type"] == "hidden" {
					return
				}
			case "img":
				if !n.has("alt") {
					t.Errorf("img %q has no alt attribute", n.attrs["src"])
				}
				return
			default:
				return
			}
			if n.accessibleName(p.ids) == "" {
				t.Errorf("<%s%s> has no accessible name", n.tag, attrSummary(n))
			}
		})
	})
}

func attrSummary(n *node) string {
	for _, k := range []string{"class", "href", "name", "id"} {
		if v := n.attrs[k]; v != "" {
			return " " + k + `="` + v + `"`
		}
	}
	return ""
}

func TestGoldens_NoPositiveTabindexAndTheOnlyFocusableScrollersAreNamed(t *testing.T) {
	eachPage(t, false, func(t *testing.T, p goldenPage) {
		p.root.walk(func(n *node) {
			v, ok := n.attrs["tabindex"]
			if !ok {
				return
			}
			if i, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || i > 0 {
				t.Errorf("<%s> has tabindex=%q: only 0 and -1 keep the natural order", n.tag, v)
			}
			if v == "0" && n.accessibleName(p.ids) == "" {
				t.Errorf("<%s> is focusable by tabindex=0 but has no accessible name", n.tag)
			}
		})
	})
}

func TestGoldens_EveryTableHasACaptionAndColumnHeaders(t *testing.T) {
	eachPage(t, false, func(t *testing.T, p goldenPage) {
		for _, tb := range p.root.find("table") {
			captioned := strings.TrimSpace(tb.attrs["aria-label"]) != ""
			for _, c := range tb.find("caption") {
				if strings.TrimSpace(c.content()) != "" {
					captioned = true
				}
			}
			if !captioned {
				t.Error("a <table> has neither a <caption> nor an aria-label")
			}
			for _, th := range tb.find("th") {
				if th.attrs["scope"] == "" {
					t.Errorf("<th> %q has no scope", strings.TrimSpace(th.content()))
				}
			}
		}
	})
}

func TestGoldens_IDsAreUniqueAndReferencesResolve(t *testing.T) {
	eachPage(t, false, func(t *testing.T, p goldenPage) {
		for _, id := range p.dups {
			t.Errorf("id %q appears more than once", id)
		}
		p.root.walk(func(n *node) {
			for _, attr := range []string{"aria-labelledby", "aria-describedby", "aria-controls"} {
				for _, ref := range strings.Fields(n.attrs[attr]) {
					if _, ok := p.ids[ref]; !ok {
						t.Errorf("<%s> %s points at missing id %q", n.tag, attr, ref)
					}
				}
			}
		})
	})
}
