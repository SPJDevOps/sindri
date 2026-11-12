package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
)

func scanEv(id string, at time.Time, meta string) evidence.Evidence {
	return evidence.Evidence{ID: id, Type: model.EvidenceScan, CreatedAt: at, Metadata: json.RawMessage(meta)}
}

func TestNewVulnPanels_ChoosesAndRendersFindings(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	counts := `"severity_counts":{"CRITICAL":1,"LOW":1}`

	t.Run("newest scan per platform wins", func(t *testing.T) {
		got := newVulnPanels([]evidence.Evidence{
			scanEv("a", t1, `{"producer":"sindri","platform":"linux/arm64",`+counts+`}`),
			scanEv("b", t0, `{"producer":"sindri","platform":"linux/amd64",`+counts+`}`),
			scanEv("c", t1, `{"producer":"sindri","platform":"linux/amd64",`+counts+`}`),
			scanEv("d", t0, `{"producer":"sindri","platform":"linux/arm64",`+counts+`}`),
			scanEv("e", t1, `{"producer":"sindri",`+counts+`}`),
		})
		var ids []string
		for _, p := range got {
			ids = append(ids, p.Platform+"="+p.EvidenceID)
		}
		want := []string{"=e", "linux/amd64=c", "linux/arm64=a"}
		if diff := cmp.Diff(want, ids); diff != "" {
			t.Errorf("panels (-want +got):\n%s", diff)
		}
	})

	t.Run("equal times fall back to the higher id", func(t *testing.T) {
		got := newVulnPanels([]evidence.Evidence{
			scanEv("b", t0, `{"producer":"sindri",`+counts+`}`),
			scanEv("a", t0, `{"producer":"sindri",`+counts+`}`),
		})
		if len(got) != 1 || got[0].EvidenceID != "b" {
			t.Errorf("got %+v, want evidence b", got)
		}
	})

	t.Run("client scan and bad metadata make no panel", func(t *testing.T) {
		got := newVulnPanels([]evidence.Evidence{
			scanEv("a", t1, `{}`),
			scanEv("b", t1, `{"producer":"ci",`+counts+`}`),
			scanEv("c", t1, `not json`),
			scanEv("d", t1, `{"producer":"sindri"}`),
			{ID: "e", Type: model.EvidenceSBOM, CreatedAt: t1, Metadata: json.RawMessage(`{"producer":"sindri",` + counts + `}`)},
		})
		if len(got) != 0 {
			t.Errorf("got %d panels, want none", len(got))
		}
	})

	t.Run("old format entries render dashes, worst first, new marked", func(t *testing.T) {
		got := newVulnPanels([]evidence.Evidence{scanEv("a", t0, `{"producer":"sindri","db_updated_at":"2026-01-01T00:00:00Z",`+counts+`,
			"vulnerabilities":[{"id":"CVE-2","severity":"LOW"},{"id":"CVE-1","severity":"CRITICAL","package":"p","installed":"1","fixed":"2"}],
			"new_vulnerabilities":["CVE-1"]}`)})
		want := []vulnRow{
			{ID: "CVE-1", Severity: "critical", Class: "sev-critical", Package: "p", Installed: "1", Fixed: "2", New: true},
			{ID: "CVE-2", Severity: "low", Class: "sev-low", Package: "—", Installed: "—", Fixed: "—"},
		}
		if diff := cmp.Diff(want, got[0].Rows); diff != "" {
			t.Errorf("rows (-want +got):\n%s", diff)
		}
		if got[0].DBUpdatedAt != "2026-01-01T00:00:00Z" || got[0].Clean {
			t.Errorf("panel = %+v", got[0])
		}
	})

	t.Run("scan with no findings is clean", func(t *testing.T) {
		got := newVulnPanels([]evidence.Evidence{scanEv("a", t0, `{"producer":"sindri","severity_counts":{"CRITICAL":0},"vulnerabilities":[]}`)})
		if len(got) != 1 || !got[0].Clean || len(got[0].Rows) != 0 {
			t.Errorf("got %+v, want one clean panel", got)
		}
	})

	t.Run("table is capped at 500 rows with the remainder counted", func(t *testing.T) {
		var vulns []string
		for i := 0; i < maxVulnRows+7; i++ {
			vulns = append(vulns, fmt.Sprintf(`{"id":"CVE-%04d","severity":"LOW"}`, i))
		}
		got := newVulnPanels([]evidence.Evidence{scanEv("a", t0, `{"producer":"sindri","severity_counts":{"LOW":507},"vulnerabilities":[`+strings.Join(vulns, ",")+`]}`)})
		if len(got[0].Rows) != maxVulnRows || got[0].Hidden != 7 {
			t.Errorf("rows=%d hidden=%d, want %d and 7", len(got[0].Rows), got[0].Hidden, maxVulnRows)
		}
	})
}
