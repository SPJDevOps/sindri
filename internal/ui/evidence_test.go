package ui

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
)

func TestNewEvidenceRow_SummarisesOnlySindriScans(t *testing.T) {
	cases := map[string]struct {
		typ      model.EvidenceType
		metadata string
		want     *scanSummary
	}{
		"sindri scan lists severities worst first and skips zeros": {
			typ:      model.EvidenceScan,
			metadata: `{"producer":"sindri","platform":"linux/arm64","severity_counts":{"LOW":2,"CRITICAL":1,"HIGH":0,"MEDIUM":0,"UNKNOWN":4}}`,
			want: &scanSummary{Platform: "linux/arm64", Counts: []severityCount{
				{Severity: "critical", Class: "sev-critical", N: 1},
				{Severity: "low", Class: "sev-low", N: 2},
				{Severity: "unknown", Class: "sev-unknown", N: 4},
			}},
		},
		"sindri scan with no findings is clean": {
			typ:      model.EvidenceScan,
			metadata: `{"producer":"sindri","severity_counts":{"CRITICAL":0,"HIGH":0,"MEDIUM":0,"LOW":0,"UNKNOWN":0}}`,
			want:     &scanSummary{Clean: true},
		},
		"client-attached scan gets no summary": {
			typ:      model.EvidenceScan,
			metadata: `{}`,
		},
		"scan claiming another producer gets no summary": {
			typ:      model.EvidenceScan,
			metadata: `{"producer":"ci","severity_counts":{"CRITICAL":0}}`,
		},
		"sindri scan without counts gets no summary": {
			typ:      model.EvidenceScan,
			metadata: `{"producer":"sindri"}`,
		},
		"unreadable metadata gets no summary": {
			typ:      model.EvidenceScan,
			metadata: `not json`,
		},
		"sindri sbom gets no summary": {
			typ:      model.EvidenceSBOM,
			metadata: `{"producer":"sindri","severity_counts":{"CRITICAL":1}}`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			row := newEvidenceRow(evidence.Evidence{Type: tc.typ, Metadata: json.RawMessage(tc.metadata)})
			if diff := cmp.Diff(tc.want, row.Scan); diff != "" {
				t.Errorf("summary mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
