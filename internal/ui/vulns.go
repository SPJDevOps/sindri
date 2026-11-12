package ui

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
)

// maxVulnRows bounds one panel's table so the page stays small however many
// findings a scan has. The full report is evidence in the API.
const maxVulnRows = 500

// scanMetadata is what the worker writes on a Sindri scan's evidence that the
// page reads (internal/scan evidenceMetadata). Pointer-free on purpose: older
// scans lack the detail fields, which then render as dashes.
type scanMetadata struct {
	Producer           string         `json:"producer"`
	Platform           string         `json:"platform"`
	DBUpdatedAt        string         `json:"db_updated_at"`
	SeverityCounts     map[string]int `json:"severity_counts"`
	Vulnerabilities    []vulnMetadata `json:"vulnerabilities"`
	NewVulnerabilities []string       `json:"new_vulnerabilities"`
}

type vulnMetadata struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
}

// parseSindriScan reads the metadata of a scan Sindri's own worker produced.
// It reports false for anything else, or for metadata it cannot read: a
// client-attached scan says nothing the page should present as Sindri's.
func parseSindriScan(ev evidence.Evidence) (scanMetadata, bool) {
	var meta scanMetadata
	if ev.Type != model.EvidenceScan || len(ev.Metadata) == 0 {
		return meta, false
	}
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil || meta.Producer != "sindri" || meta.SeverityCounts == nil {
		return scanMetadata{}, false
	}
	return meta, true
}

// severityCounts lists only the severities with findings, worst first.
func severityCounts(counts map[string]int) []severityCount {
	var out []severityCount
	for _, sev := range summarySeverities {
		if n := counts[sev]; n > 0 {
			out = append(out, severityCount{Severity: strings.ToLower(sev), Class: "sev-" + strings.ToLower(sev), N: n})
		}
	}
	return out
}

// vulnPanel is the newest Sindri scan of one platform.
type vulnPanel struct {
	Platform    string
	EvidenceID  string
	ScannedAt   time.Time
	DBUpdatedAt string
	Counts      []severityCount
	Clean       bool
	Rows        []vulnRow
	Hidden      int
}

type vulnRow struct {
	ID        string
	Severity  string
	Class     string
	Package   string
	Installed string
	Fixed     string
	New       bool
}

const noValue = "—"

func orDash(s string) string {
	if s == "" {
		return noValue
	}
	return s
}

func severityRank(sev string) int {
	if i := slices.Index(summarySeverities, strings.ToUpper(sev)); i >= 0 {
		return i
	}
	return len(summarySeverities)
}

// newVulnPanels builds one panel per platform from the newest Sindri scan of
// that platform, ordered as the severity gate picks it: created_at, then id
// (ListNewestSindriScans). Platforms sort by name, so the no-platform scan of
// a single-image subject comes first. It reads only evidence metadata.
func newVulnPanels(evs []evidence.Evidence) []vulnPanel {
	type pick struct {
		ev   evidence.Evidence
		meta scanMetadata
	}
	newest := map[string]pick{}
	for _, ev := range evs {
		meta, ok := parseSindriScan(ev)
		if !ok {
			continue
		}
		cur, seen := newest[meta.Platform]
		if !seen || ev.CreatedAt.After(cur.ev.CreatedAt) || (ev.CreatedAt.Equal(cur.ev.CreatedAt) && ev.ID > cur.ev.ID) {
			newest[meta.Platform] = pick{ev, meta}
		}
	}
	platforms := make([]string, 0, len(newest))
	for p := range newest {
		platforms = append(platforms, p)
	}
	slices.Sort(platforms)

	panels := make([]vulnPanel, 0, len(platforms))
	for _, p := range platforms {
		pk := newest[p]
		panel := vulnPanel{
			Platform: p, EvidenceID: pk.ev.ID, ScannedAt: pk.ev.CreatedAt, DBUpdatedAt: pk.meta.DBUpdatedAt,
			Counts: severityCounts(pk.meta.SeverityCounts),
		}
		panel.Clean = len(panel.Counts) == 0 && len(pk.meta.Vulnerabilities) == 0
		isNew := map[string]bool{}
		for _, id := range pk.meta.NewVulnerabilities {
			isNew[id] = true
		}
		rows := make([]vulnRow, 0, len(pk.meta.Vulnerabilities))
		for _, v := range pk.meta.Vulnerabilities {
			sev := strings.ToLower(v.Severity)
			rows = append(rows, vulnRow{
				ID: v.ID, Severity: sev, Class: "sev-" + sev, New: isNew[v.ID],
				Package: orDash(v.Package), Installed: orDash(v.Installed), Fixed: orDash(v.Fixed),
			})
		}
		slices.SortStableFunc(rows, func(a, b vulnRow) int {
			if c := severityRank(a.Severity) - severityRank(b.Severity); c != 0 {
				return c
			}
			if c := strings.Compare(a.ID, b.ID); c != 0 {
				return c
			}
			return strings.Compare(a.Package, b.Package)
		})
		if len(rows) > maxVulnRows {
			panel.Hidden = len(rows) - maxVulnRows
			rows = rows[:maxVulnRows]
		}
		panel.Rows = rows
		panels = append(panels, panel)
	}
	return panels
}
