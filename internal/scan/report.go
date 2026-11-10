package scan

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// severities lists the keys present in every severity_counts object.
var severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// trivyReport is the part of Trivy's JSON report (SchemaVersion 2) the
// scanner reads.
type trivyReport struct {
	Results []struct {
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
	if err := json.NewDecoder(r).Decode(&report); err != nil {
		return nil, fmt.Errorf("decoding trivy report: %w", err)
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
