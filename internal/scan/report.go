package scan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// reportSchemaVersion is the Trivy JSON schema this parser understands.
const reportSchemaVersion = 2

// severities lists the keys present in every severity_counts object.
var severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// trivyReport is the part of Trivy's JSON report (SchemaVersion 2) the
// scanner reads.
type trivyReport struct {
	SchemaVersion int `json:"SchemaVersion"`
	Results       []struct {
		Vulnerabilities []struct {
			VulnerabilityID string `json:"VulnerabilityID"`
			Severity        string `json:"Severity"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// countSeverities reads a Trivy JSON report and counts vulnerabilities per
// severity. Every key is present; a severity Trivy does not name counts as
// UNKNOWN.
func countSeverities(r io.Reader) (map[string]int, error) {
	var report trivyReport
	dec := json.NewDecoder(r)
	if err := dec.Decode(&report); err != nil {
		return nil, fmt.Errorf("decoding trivy report: %w", err)
	}
	// Anything that is not a schema 2 report is an error, never zero counts:
	// a gate must not read "clean" out of a document it did not understand.
	if report.SchemaVersion != reportSchemaVersion {
		return nil, fmt.Errorf("trivy report has schema version %d, want %d", report.SchemaVersion, reportSchemaVersion)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trivy report has trailing data")
	}
	counts := make(map[string]int, len(severities))
	for _, s := range severities {
		counts[s] = 0
	}
	for _, res := range report.Results {
		for _, v := range res.Vulnerabilities {
			sev := strings.ToUpper(v.Severity)
			if _, ok := counts[sev]; !ok {
				sev = "UNKNOWN"
			}
			counts[sev]++
		}
	}
	return counts, nil
}
