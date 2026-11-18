package ui

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// The rows of the repository and package pages, and the figures they share.
// A row's verdict, its highest approved stage and its severity summary, is
// built here from the batch summaries the lifecycle service returns, so a tag,
// a version, a repository's package, and (later) an artifact's header all read
// one set of rules.

// severityView is what the severity-pills partial renders: the severities
// with findings, worst first, and whether a Sindri scan exists to say there
// are none. No scan shows as "not scanned", never as clean.
type severityView struct {
	Counts  []severityCount
	Scanned bool
}

// scanCounts adds up the findings of one subject's newest Sindri scans, one per
// platform. Scans a client attached never reach it (ADR 29): the summary query
// returns only Sindri's own, and parseSindriScan checks the producer again.
func scanCounts(scans []json.RawMessage) (counts map[string]int, scanned bool) {
	counts = map[string]int{}
	for _, raw := range scans {
		meta, ok := parseSindriScan(evidence.Evidence{Type: model.EvidenceScan, Metadata: raw})
		if !ok {
			continue
		}
		scanned = true
		for sev, n := range meta.SeverityCounts {
			counts[strings.ToUpper(sev)] += n
		}
	}
	return counts, scanned
}

// severityOf summarises what the Sindri scans of several subjects found: the
// counts added up, and Scanned only when every subject has a scan. A version
// with two files is scanned only if both are; a subject nobody scanned must
// not make the whole look clean.
func severityOf(sums []lifecycle.SubjectSummary) severityView {
	total := map[string]int{}
	scanned := len(sums) > 0
	for _, s := range sums {
		counts, ok := scanCounts(s.Scans)
		if !ok {
			scanned = false
		}
		for sev, n := range counts {
			total[sev] += n
		}
	}
	return severityView{Counts: severityCounts(total), Scanned: scanned}
}

// worstOf reduces a summary to its single worst severity, with the number of
// findings at that severity, for a row that has room for one pill.
func worstOf(v severityView) severityView {
	if len(v.Counts) > 1 {
		v.Counts = v.Counts[:1]
	}
	return v
}

// stageOf is the highest stage every one of sums is currently approved at in
// repo, or "" when any of them is approved at none. A version with two files
// is promoted only as far as its least promoted file.
func stageOf(sums []lifecycle.SubjectSummary, repo string) string {
	if len(sums) == 0 {
		return ""
	}
	var low *lifecycle.StageApproval
	for _, s := range sums {
		var best *lifecycle.StageApproval
		for i, a := range s.Approvals {
			if a.Repository == repo && (best == nil || a.Position > best.Position) {
				best = &s.Approvals[i]
			}
		}
		if best == nil {
			return ""
		}
		if low == nil || best.Position < low.Position {
			low = best
		}
	}
	return low.Stage
}

// ownersOf lists the owners of sums the caller may see: those naming a
// repository they may read, as the artifact page shows them.
func ownersOf(sums []lifecycle.SubjectSummary, visible map[string]bool) []string {
	var out []string
	for _, s := range sums {
		for _, o := range s.Owners {
			if name, ok := strings.CutPrefix(o, registry.OwnerPrefix); ok && visible[name] && !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	slices.Sort(out)
	return out
}

// pickSummaries returns the summaries of digests, skipping any the query had no
// blob for.
func pickSummaries(all map[model.Digest]lifecycle.SubjectSummary, digests []model.Digest) []lifecycle.SubjectSummary {
	out := make([]lifecycle.SubjectSummary, 0, len(digests))
	for _, d := range digests {
		if s, ok := all[d]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Sorts of a repository's package list.
const (
	sortName      = "name"
	sortPublished = "published"
)

var packageSorts = []optionView{
	{Value: sortName, Label: "Name"},
	{Value: sortPublished, Label: "Last publish"},
}

// pkgFilter is the package list's state as the URL carries it, parsed with the
// same rules as the repository list's: anything unknown is dropped.
type pkgFilter struct {
	Q    string // case-insensitive substring of the name
	Sort string // sortName or sortPublished
	Page int    // 1-based; 0 means the first
}

func parsePkgFilter(v url.Values) pkgFilter {
	f := pkgFilter{Sort: sortName}
	q := strings.TrimSpace(strings.ToValidUTF8(v.Get("q"), ""))
	if utf8.RuneCountInString(q) > maxFilterLen {
		q = string([]rune(q)[:maxFilterLen])
	}
	f.Q = q
	if v.Get("sort") == sortPublished {
		f.Sort = sortPublished
	}
	if n, err := strconv.Atoi(v.Get("page")); err == nil && n > 1 {
		// A page past the end shows the last one, so any number beyond what
		// a registry could fill is as good as maxPage.
		f.Page = min(n, maxPage)
	}
	return f
}

// maxPage bounds the page number a URL may ask for, so the row offset fits
// the query's int32 whatever the URL says.
const maxPage = 100_000

// Active reports whether the filter narrows the list. The sort and the page do
// not.
func (f pkgFilter) Active() bool { return f.Q != "" }

// href renders the filter as a URL on the repository page, leaving out every
// default.
func (f pkgFilter) href(repo string) string {
	q := url.Values{}
	if f.Q != "" {
		q.Set("q", f.Q)
	}
	if f.Sort != sortName {
		q.Set("sort", f.Sort)
	}
	if f.Page > 1 {
		q.Set("page", strconv.Itoa(f.Page))
	}
	if len(q) == 0 {
		return repoHref(repo)
	}
	return repoHref(repo) + "?" + q.Encode()
}

func (f pkgFilter) registrySort() registry.PackageSort {
	if f.Sort == sortPublished {
		return registry.SortByPublished
	}
	return registry.SortByName
}

// pkgRow is one package of a repository page.
type pkgRow struct {
	Name     string
	Href     string
	Latest   string
	Versions int64
	// Published is the last publish, zero when none is dated.
	Published   time.Time
	Severity    severityView
	HasSeverity bool
}

// repoFacts is the header facts block of a repository page.
type repoFacts struct {
	// Stages are the lifecycle stages that apply to a hosted repository, in
	// position order: every stage the server defines, or those the
	// repository's lifecycle restricts it to.
	Stages    []string
	NoStages  bool // hosted, and the server defines none that apply
	IsHosted  bool
	IsProxy   bool
	Entries   int64
	LastFetch time.Time
}
