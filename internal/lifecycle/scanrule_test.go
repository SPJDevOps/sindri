package lifecycle

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

func TestEvaluateScanRule_NamesEachCVEOnce(t *testing.T) {
	digest := model.Digest{}
	cases := []struct {
		name     string
		metadata string
		want     []Finding
	}{
		{"SameCVEInTwoPackagesIsOneFinding",
			`{"vulnerabilities":[
				{"id":"CVE-1","severity":"HIGH","package":"zlib","installed":"1.0"},
				{"id":"CVE-1","severity":"HIGH","package":"openssl","installed":"3.0","fixed":"3.1"}]}`,
			[]Finding{{ID: "CVE-1", Severity: "HIGH"}}},
		{"SameCVEAtTwoSeveritiesIsKeptTwice",
			`{"vulnerabilities":[{"id":"CVE-1","severity":"HIGH"},{"id":"CVE-1","severity":"CRITICAL"}]}`,
			[]Finding{{ID: "CVE-1", Severity: "HIGH"}, {ID: "CVE-1", Severity: "CRITICAL"}}},
		{"OrderIsPreserved",
			`{"vulnerabilities":[{"id":"CVE-2","severity":"HIGH","package":"a"},{"id":"CVE-1","severity":"HIGH","package":"a"},{"id":"CVE-2","severity":"HIGH","package":"b"}]}`,
			[]Finding{{ID: "CVE-2", Severity: "HIGH"}, {ID: "CVE-1", Severity: "HIGH"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := db.ListNewestSindriScansRow{ID: uuid.New(), Metadata: []byte(tc.metadata), CreatedAt: time.Now()}
			scans := append(make([]db.ListNewestSindriScansRow, 0, 2), row)
			rule := GateRule{Scan: &ScanRule{MaxSeverity: SeverityMedium}}
			got, err := evaluateScanRule(rule, digest, scans, func(uuid.UUID) {})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || len(got[0].Findings) != len(tc.want) {
				t.Fatalf("violations = %+v, want one naming %v", got, tc.want)
			}
			for i, f := range tc.want {
				if got[0].Findings[i] != f {
					t.Errorf("finding %d = %v, want %v", i, got[0].Findings[i], f)
				}
			}
		})
	}
}
