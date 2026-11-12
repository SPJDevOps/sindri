package scan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// reportSchemaVersion is the Trivy JSON schema this parser understands.
const reportSchemaVersion = 2

// severities lists the keys present in every severity_counts object.
var severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// severityRank orders severities worst first; it indexes severities.
func severityRank(s string) int {
	for i, name := range severities {
		if name == s {
			return i
		}
	}
	return len(severities)
}

// trivyReport is the part of Trivy's JSON report (SchemaVersion 2) the
// scanner reads.
type trivyReport struct {
	SchemaVersion int `json:"SchemaVersion"`
	Results       []struct {
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			Severity         string `json:"Severity"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// vulnerability is one finding in the evidence metadata: a vulnerability in one
// installed package. The severity gate reads only id and severity (ADR 30); the
// package detail is for people and for the re-scan comparison (ADR 31).
type vulnerability struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed,omitempty"`
}

// vulnKey identifies a finding for de-duplication; the fixed version is
// detail, not identity.
type vulnKey struct{ id, severity, pkg, installed string }

// countSeverities reads a Trivy JSON report and counts vulnerabilities per
// severity. Every key is present; a severity Trivy does not name counts as
// UNKNOWN; the counts are per Trivy entry. It also returns the findings,
// de-duplicated by (id, severity, package, installed) and sorted by severity
// descending, then id, package, installed; never nil, so the metadata always
// carries the list.
func countSeverities(r io.Reader) (map[string]int, []vulnerability, error) {
	var report trivyReport
	dec := json.NewDecoder(r)
	if err := dec.Decode(&report); err != nil {
		return nil, nil, fmt.Errorf("decoding trivy report: %w", err)
	}
	// Anything that is not a schema 2 report is an error, never zero counts:
	// a gate must not read "clean" out of a document it did not understand.
	if report.SchemaVersion != reportSchemaVersion {
		return nil, nil, fmt.Errorf("trivy report has schema version %d, want %d", report.SchemaVersion, reportSchemaVersion)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trivy report has trailing data")
	}
	counts := make(map[string]int, len(severities))
	for _, s := range severities {
		counts[s] = 0
	}
	found := make(map[vulnKey]vulnerability)
	for _, res := range report.Results {
		for _, v := range res.Vulnerabilities {
			sev := strings.ToUpper(v.Severity)
			if _, ok := counts[sev]; !ok {
				sev = "UNKNOWN"
			}
			counts[sev]++
			key := vulnKey{v.VulnerabilityID, sev, v.PkgName, v.InstalledVersion}
			if _, seen := found[key]; !seen {
				found[key] = vulnerability{
					ID: v.VulnerabilityID, Severity: sev, Package: v.PkgName,
					Installed: v.InstalledVersion, Fixed: v.FixedVersion,
				}
			}
		}
	}
	vulns := make([]vulnerability, 0, len(found))
	for _, v := range found {
		vulns = append(vulns, v)
	}
	sort.Slice(vulns, func(i, j int) bool {
		if ri, rj := severityRank(vulns[i].Severity), severityRank(vulns[j].Severity); ri != rj {
			return ri < rj
		}
		a, b := vulns[i], vulns[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Installed < b.Installed
	})
	return counts, vulns, nil
}
