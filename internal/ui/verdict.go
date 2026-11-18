package ui

import (
	"slices"
	"strings"
	"time"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
)

// The verdict strip of a hosted artifact page: where the digest stands in this
// repository's lifecycle, what Sindri's own scans found, how old that is, and
// whether the findings got worse after someone approved it. Everything is
// derived from the subject and evidence the page already loaded, so the strip
// costs no query. A scan a client attached never counts (ADR 29), and a digest
// nobody scanned reads "not scanned", never clean (ADR 30).

// verdictView is what the strip renders.
type verdictView struct {
	// Stage is the stage the verdict names: the highest approved stage, or with
	// Revoked set, the stage the subject was revoked from. Empty means not
	// promoted.
	Stage   string
	Revoked bool
	// Severity is the worst severity with its count; Scanned is false when
	// Sindri has not scanned the digest.
	Severity severityView
	// ScannedAt is the newest Sindri scan; zero when there is none.
	ScannedAt time.Time
	// NewFindings is set when the newest scan came after the approval and found
	// worse than the scan before it.
	NewFindings bool
}

// verdictStage reads one repository's lifecycle state. The highest stage the
// subject ever reached there, approved or revoked, decides: if the latest
// action on it is a revoke the subject is "revoked from" it, even when a lower
// stage is still approved, because the revoke is what a reader needs to see.
// Otherwise the stage is the highest one approved. approvedAt is the time of
// the approval that stage names; zero unless the subject is approved.
func verdictStage(states []lifecycle.State, repo string, position map[string]int32) (stage string, revoked bool, approvedAt time.Time) {
	var rs []stageReach
	for _, st := range states {
		if st.Repository != repo {
			continue
		}
		pos, known := position[st.Stage]
		if !known {
			continue
		}
		rs = append(rs, stageReach{Stage: st.Stage, Position: pos, Revoked: st.Action == lifecycle.ActionRevoke})
	}
	top, ok := topReach(rs)
	if !ok {
		return "", false, time.Time{}
	}
	if top.Revoked {
		return top.Stage, true, time.Time{}
	}
	for _, st := range states {
		if st.Repository == repo && st.Stage == top.Stage {
			return top.Stage, false, st.At
		}
	}
	return top.Stage, false, time.Time{}
}

// scanPoint is one Sindri scan reduced to what the strip compares.
type scanPoint struct {
	ID        string
	At        time.Time
	Counts    map[string]int
	Platforms []string
}

// sindriScans groups a subject's Sindri scans by platform, oldest first with
// id breaking ties as the gate does.
func sindriScans(evs []evidence.Evidence) map[string][]scanPoint {
	byPlatform := map[string][]scanPoint{}
	for _, ev := range evs {
		meta, ok := parseSindriScan(ev)
		if !ok {
			continue
		}
		counts := make(map[string]int, len(meta.SeverityCounts))
		for sev, n := range meta.SeverityCounts {
			counts[strings.ToUpper(sev)] = n
		}
		byPlatform[meta.Platform] = append(byPlatform[meta.Platform], scanPoint{ID: ev.ID, At: ev.CreatedAt, Counts: counts, Platforms: meta.Platforms})
	}
	for _, scans := range byPlatform {
		slices.SortFunc(scans, func(a, b scanPoint) int {
			if c := a.At.Compare(b.At); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	return byPlatform
}

// gravest are the severities whose count growth is "new findings" by itself.
var gravest = []string{"CRITICAL", "HIGH"}

// worse reports whether now found strictly more at CRITICAL or HIGH than
// before. A missing earlier scan is an empty one, so a first scan counts when it
// has any CRITICAL or HIGH finding. Growth in MEDIUM, LOW, or UNKNOWN alone does
// not.
func worse(now, before map[string]int) bool {
	for _, sev := range gravest {
		if now[sev] > before[sev] {
			return true
		}
	}
	return false
}

// newFindingsSince reports whether, on any platform, the newest scan was made
// after approvedAt and has strictly more CRITICAL or HIGH findings than the scan
// before it (or any, with no scan before). With no approval there is nothing to
// be new since.
func newFindingsSince(scans map[string][]scanPoint, approvedAt time.Time) bool {
	if approvedAt.IsZero() {
		return false
	}
	for _, list := range scans {
		last := list[len(list)-1]
		if !last.At.After(approvedAt) {
			continue
		}
		var before map[string]int
		if len(list) > 1 {
			before = list[len(list)-2].Counts
		}
		if worse(last.Counts, before) {
			return true
		}
	}
	return false
}

// newVerdict builds the strip for one repository.
func newVerdict(states []lifecycle.State, repo string, position map[string]int32, evs []evidence.Evidence) verdictView {
	stage, revoked, approvedAt := verdictStage(states, repo, position)
	v := verdictView{Stage: stage, Revoked: revoked}
	scans := sindriScans(evs)
	total := map[string]int{}
	var set scanSet
	// In platform order, as the gate reads them, so a tie names the same scan.
	platforms := make([]string, 0, len(scans))
	for p := range scans {
		platforms = append(platforms, p)
	}
	slices.Sort(platforms)
	for _, p := range platforms {
		list := scans[p]
		last := list[len(list)-1]
		set.add(p, last.Platforms, last.At)
		for sev, n := range last.Counts {
			total[sev] += n
		}
		if last.At.After(v.ScannedAt) {
			v.ScannedAt = last.At
		}
	}
	v.Severity = worstOf(severityView{Counts: severityCounts(total), Scanned: set.complete()})
	v.NewFindings = newFindingsSince(scans, approvedAt)
	return v
}
