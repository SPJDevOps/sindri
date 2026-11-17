package ui

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// The repository list is filtered, grouped, and paged on the server, from the
// URL alone: every control is a plain GET form field or link, so the page works
// without a script and any state can be bookmarked. Everything the caller may
// not read is dropped before the first count is taken (invariant: a total, a
// group count, or "N of M" never includes a repository the caller cannot read).

const (
	// maxFilterLen bounds the text filter, in characters.
	maxFilterLen = 100
	// repoPageSize is how many repositories one page of the list holds.
	repoPageSize = 200
	// tableAbove is the number of listed repositories above which the table is
	// the default view; at or below it, tiles are.
	tableAbove = 30

	viewTable = "table"
	viewTiles = "tiles"
)

var (
	repoTypes     = []string{string(model.RepoTypeHosted), string(model.RepoTypeProxy), string(model.RepoTypeVirtual)}
	repoProtocols = []string{string(model.ProtocolOCI), string(model.ProtocolPyPI), string(model.ProtocolNPM)}
)

// repoFilter is the list's state as the URL carries it. A zero field means
// "not set". Anything the URL holds that is not a known value is dropped, never
// an error: the page just ignores what it does not understand.
type repoFilter struct {
	Q        string // case-insensitive substring of the name
	Type     string // one of repoTypes
	Protocol string // one of repoProtocols
	View     string // viewTable or viewTiles; empty picks by size
	Page     int    // 1-based; 0 means the first
}

func parseRepoFilter(v url.Values) repoFilter {
	var f repoFilter
	q := strings.TrimSpace(strings.ToValidUTF8(v.Get("q"), ""))
	if utf8.RuneCountInString(q) > maxFilterLen {
		q = string([]rune(q)[:maxFilterLen])
	}
	f.Q = q
	if t := v.Get("type"); slices.Contains(repoTypes, t) {
		f.Type = t
	}
	if p := v.Get("protocol"); slices.Contains(repoProtocols, p) {
		f.Protocol = p
	}
	if w := v.Get("view"); w == viewTable || w == viewTiles {
		f.View = w
	}
	if n, err := strconv.Atoi(v.Get("page")); err == nil && n > 1 {
		f.Page = n
	}
	return f
}

// Active reports whether the filter narrows the list at all. The view and the
// page do not: they change how the same set is shown.
func (f repoFilter) Active() bool { return f.Q != "" || f.Type != "" || f.Protocol != "" }

func (f repoFilter) matches(repo model.Repository) bool {
	if f.Type != "" && string(repo.Type) != f.Type {
		return false
	}
	if f.Protocol != "" && string(repo.Protocol) != f.Protocol {
		return false
	}
	return f.Q == "" || strings.Contains(strings.ToLower(repo.Name), strings.ToLower(f.Q))
}

// href renders the filter as a URL on the list, leaving out every default so
// the plain list stays /ui/.
func (f repoFilter) href() string {
	q := url.Values{}
	if f.Q != "" {
		q.Set("q", f.Q)
	}
	if f.Type != "" {
		q.Set("type", f.Type)
	}
	if f.Protocol != "" {
		q.Set("protocol", f.Protocol)
	}
	if f.View != "" {
		q.Set("view", f.View)
	}
	if f.Page > 1 {
		q.Set("page", strconv.Itoa(f.Page))
	}
	if len(q) == 0 {
		return Prefix + "/"
	}
	return Prefix + "/?" + q.Encode()
}

// useTable picks the view: the one the URL names, else the table above
// tableAbove listed repositories and tiles at or below it. The count is of the
// repositories the filter keeps, so a narrow filter on a big registry is tiles.
func useTable(listed int, view string) bool {
	switch view {
	case viewTable:
		return true
	case viewTiles:
		return false
	}
	return listed > tableAbove
}

// namespace is the group a repository is listed under: the part of its name
// before the first slash, or "" for a name with none.
func namespace(name string) string {
	ns, _, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	return ns
}

// sortRepos orders repositories for listing: names without a namespace first,
// then by namespace, then by name. Paging cuts this order, so a group is never
// split between pages except at a page boundary.
func sortRepos(repos []model.Repository) {
	slices.SortFunc(repos, func(a, b model.Repository) int {
		if c := strings.Compare(namespace(a.Name), namespace(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// paginate clamps page to 1..pages and returns the half-open slice bounds.
func paginate(total, page, size int) (clamped, pages, from, to int) {
	pages = max(1, (total+size-1)/size)
	clamped = min(max(page, 1), pages)
	from = (clamped - 1) * size
	to = min(from+size, total)
	return clamped, pages, from, to
}

// rowStats are the figures of one repository; see registry.RepositoryStats.
type rowStats struct {
	Packages      int64
	LastPublished time.Time
}

type repoRow struct {
	repoView
	// Detail is the proxy's upstream host or the virtual repository's member
	// count; empty for a hosted repository.
	Detail string
	// Stats is nil for a virtual repository, which holds nothing of its own.
	Stats *rowStats
}

type repoGroup struct {
	Label string // empty for names without a namespace
	Rows  []repoRow
}

// groupRows splits rows, already in sortRepos order, into one group per
// namespace. The group without a namespace comes first when present.
func groupRows(rows []repoRow) []repoGroup {
	var groups []repoGroup
	for _, row := range rows {
		ns := namespace(row.Name)
		if n := len(groups); n == 0 || groups[n-1].Label != ns {
			groups = append(groups, repoGroup{Label: ns})
		}
		g := &groups[len(groups)-1]
		g.Rows = append(g.Rows, row)
	}
	return groups
}

// upstreamHost is the host (and port) of an upstream URL, which is all the list
// shows of it: never a path, and never credentials.
func upstreamHost(raw string) string {
	// url.URL keeps userinfo apart from Host, so Host never carries credentials.
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func memberDetail(n int) string {
	switch n {
	case 0:
		return "resolves no member you may read"
	case 1:
		return "resolves 1 member"
	default:
		return fmt.Sprintf("resolves %d members", n)
	}
}

func upstreamDetail(host string) string {
	if host == "" {
		return "caches an upstream"
	}
	return "caches " + host
}

type optionView struct {
	Value, Label string
	Selected     bool
}

type pagerView struct {
	Page, Pages int
	Prev, Next  string // empty when there is no such page
}

type reposPage struct {
	chrome
	// Total is the repositories the caller may read; Matching those the filter
	// keeps. Both are counted after the readability filter.
	Total, Matching int
	// From and To are the 1-based positions of the first and last row shown.
	From, To int
	Filter   repoFilter
	Table    bool
	Groups   []repoGroup
	Types    []optionView
	Protos   []optionView
	// TableHref and TilesHref switch the view and keep the filter.
	TableHref, TilesHref string
	// ClearHref drops the filter and keeps the view.
	ClearHref string
	Pager     *pagerView
}

func options(values []string, selected string) []optionView {
	out := make([]optionView, len(values))
	for i, v := range values {
		out[i] = optionView{Value: v, Label: v, Selected: v == selected}
	}
	return out
}

// statsView converts the registry's figures for one repository.
func statsView(s registry.RepositoryStats) *rowStats {
	return &rowStats{Packages: s.Packages, LastPublished: s.LastPublished}
}
