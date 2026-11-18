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
// are none. No scan, or a scan of only part of an index, shows as "not
// scanned", never as clean.
type severityView struct {
	Counts  []severityCount
	Scanned bool
}

// scanSet is the newest Sindri scan of each platform of one subject, with what
// the verdict and the rows both need to say whether the subject is fully
// scanned. It mirrors the severity gate (lifecycle evaluateScanRule, ADR 30):
// the newest scan overall lists the platforms of its index, and each of them
// must have a scan of its own.
type scanSet struct {
	have map[string]bool
	// required is the platforms the newest scan lists; at is that scan's time.
	required []string
	at       time.Time
	set      bool
}

// add records the newest scan of one platform. Scans must arrive in platform
// order, as the queries return them, so that on a tie in time the first
// platform names the required set, as the gate's loop does.
func (s *scanSet) add(platform string, platforms []string, at time.Time) {
	if s.have == nil {
		s.have = map[string]bool{}
	}
	s.have[platform] = true
	if !s.set || at.After(s.at) {
		s.set, s.at, s.required = true, at, platforms
	}
}

// complete reports whether there is a scan and every platform the newest one
// lists has one. A subject nobody scanned is not complete.
func (s *scanSet) complete() bool {
	if !s.set {
		return false
	}
	for _, p := range s.required {
		if !s.have[p] {
			return false
		}
	}
	return true
}

// scanCounts adds up the findings of one subject's newest Sindri scans, one per
// platform. Scans a client attached never reach it (ADR 29): the summary query
// returns only Sindri's own, and parseSindriScan checks the producer again.
// scanned is false when no scan exists or the newest scan lists a platform that
// has none, so a half-scanned index cannot read as clean; the counts still add
// up whatever was found.
func scanCounts(scans []json.RawMessage) (counts map[string]int, scanned bool) {
	counts = map[string]int{}
	var set scanSet
	for _, raw := range scans {
		meta, ok := parseSindriScan(evidence.Evidence{Type: model.EvidenceScan, Metadata: raw})
		if !ok {
			continue
		}
		var when struct {
			At time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &when); err != nil {
			continue
		}
		set.add(meta.Platform, meta.Platforms, when.At)
		for sev, n := range meta.SeverityCounts {
			counts[strings.ToUpper(sev)] += n
		}
	}
	return counts, set.complete()
}

// severityOf summarises what the Sindri scans of several subjects found: the
// counts added up, and Scanned only when every subject is fully scanned. A
// version with two files is scanned only if both are; a subject nobody scanned
// must not make the whole look clean.
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

// stageView is what the stage-pill partial renders: the stage a digest has
// reached, and whether it stands revoked there. The zero value is "not
// promoted".
type stageView struct {
	Name    string
	Revoked bool
}

// stageReach is the highest stage one subject reached in a repository, and
// whether that stage now stands revoked.
type stageReach struct {
	Stage    string
	Position int32
	Revoked  bool
}

// topReach picks the highest stage reached. Every caller, the verdict strip
// and the rows alike, decides through it: the highest stage with any current
// state is the one that speaks, so a revoke of it reads "revoked from X" even
// when a lower stage is still approved. ok is false when none was reached.
func topReach(rs []stageReach) (top stageReach, ok bool) {
	for _, r := range rs {
		if !ok || r.Position > top.Position || (r.Position == top.Position && r.Revoked) {
			top, ok = r, true
		}
	}
	return top, ok
}

// stageOf is the stage a row shows for sums in repo. Each subject reads as the
// verdict strip reads it (topReach). Several subjects, the files of one
// version, combine to the least favourable: not promoted if any has reached no
// stage, else revoked from the lowest stage any was revoked from, else the
// lowest stage any is approved at.
func stageOf(sums []lifecycle.SubjectSummary, repo string) stageView {
	if len(sums) == 0 {
		return stageView{}
	}
	var low, lowRevoked *stageReach
	for _, s := range sums {
		var rs []stageReach
		for _, a := range s.Approvals {
			if a.Repository == repo {
				rs = append(rs, stageReach{Stage: a.Stage, Position: a.Position, Revoked: a.Action == lifecycle.ActionRevoke})
			}
		}
		top, ok := topReach(rs)
		if !ok {
			return stageView{}
		}
		if top.Revoked {
			if lowRevoked == nil || top.Position < lowRevoked.Position {
				lowRevoked = &top
			}
		} else if low == nil || top.Position < low.Position {
			low = &top
		}
	}
	if lowRevoked != nil {
		return stageView{Name: lowRevoked.Stage, Revoked: true}
	}
	return stageView{Name: low.Stage}
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
