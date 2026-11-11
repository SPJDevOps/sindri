//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/scanjob"
)

// gateEnv is a hosted OCI repository holding one image, and a prod stage whose
// only gate is a severity ceiling.
type gateEnv struct {
	*identityEnv
	image string
}

func newGateEnv(t *testing.T, ceiling string) *gateEnv {
	t.Helper()
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "severity-gate", nil, nil)
	env.setStages(t, `{"stages":[{"name":"prod","gates":[{"scan":{"max_severity":"`+ceiling+`"}}]}]}`)
	return &gateEnv{identityEnv: env, image: img}
}

// sindriScan attaches a scan the way the worker does: through the evidence
// service with producer set, which the HTTP API can never do. It returns the
// evidence id.
func (e *gateEnv) sindriScan(t *testing.T, platform string, findings ...lifecycle.Finding) string {
	t.Helper()
	meta := map[string]any{"producer": "sindri", "vulnerabilities": findings}
	if findings == nil {
		meta["vulnerabilities"] = []lifecycle.Finding{}
	}
	if platform != "" {
		meta["platform"] = platform
	}
	return e.attachScanMetadata(t, meta)
}

func (e *gateEnv) attachScanMetadata(t *testing.T, meta map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := model.ParseDigest(e.image)
	if err != nil {
		t.Fatal(err)
	}
	svc := evidence.New(e.database, cas.New(e.blobs), "memory")
	ev, err := svc.Attach(t.Context(), evidence.AttachParams{
		Subject: digest, Type: model.EvidenceScan, MediaType: "application/vnd.aquasec.trivy.report+json",
		Body: strings.NewReader(`{"SchemaVersion":2,"n":"` + string(raw) + `"}`), Size: -1,
		Actor: scanjob.Actor, Metadata: raw,
	})
	if err != nil {
		t.Fatalf("attaching scan: %v", err)
	}
	return ev.ID
}

func (e *gateEnv) promote(t *testing.T) httpResult {
	t.Helper()
	return e.do(t, http.MethodPost, "/api/v1/subjects/"+e.image+"/promote", `{"repository":"demo","stage":"prod","reason":"release"}`)
}

func (e *gateEnv) refusal(t *testing.T) api.ErrorBody {
	t.Helper()
	res := e.promote(t)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("promote: %d %s, want 409", res.StatusCode, res.Body)
	}
	var body api.ErrorBody
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("decoding %s: %v", res.Body, err)
	}
	if body.Code != "GATE_FAILED" {
		t.Fatalf("code = %q, want GATE_FAILED (%s)", body.Code, res.Body)
	}
	return body
}

func (e *gateEnv) mustPromote(t *testing.T) {
	t.Helper()
	if res := e.promote(t); res.StatusCode != http.StatusCreated {
		t.Fatalf("promote: %d %s, want 201", res.StatusCode, res.Body)
	}
}

func TestPromote_ScanGateRefusesFindingAboveCeiling(t *testing.T) {
	tests := []struct {
		name     string
		ceiling  string
		findings []lifecycle.Finding
		wantIDs  []string
	}{
		{"CriticalAboveHigh", "high", []lifecycle.Finding{{ID: "CVE-2026-1111", Severity: "CRITICAL"}, {ID: "CVE-2026-2222", Severity: "HIGH"}}, []string{"CVE-2026-1111"}},
		{"HighAboveMedium", "medium", []lifecycle.Finding{{ID: "CVE-2026-2222", Severity: "HIGH"}, {ID: "CVE-2026-3333", Severity: "MEDIUM"}}, []string{"CVE-2026-2222"}},
		{"MediumAboveLow", "low", []lifecycle.Finding{{ID: "CVE-2026-3333", Severity: "MEDIUM"}, {ID: "CVE-2026-4444", Severity: "LOW"}}, []string{"CVE-2026-3333"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newGateEnv(t, tc.ceiling)
			env.sindriScan(t, "", tc.findings...)

			res := env.promote(t)
			if res.StatusCode != http.StatusConflict {
				t.Fatalf("promote: %d %s, want 409", res.StatusCode, res.Body)
			}
			var body api.ErrorBody
			if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "GATE_FAILED" || len(body.Violations) != 1 {
				t.Fatalf("body = %s, want GATE_FAILED with one violation", res.Body)
			}
			v := body.Violations[0]
			var got []string
			for _, f := range v.Findings {
				got = append(got, f.ID)
			}
			if v.Rule != "scan.max_severity="+tc.ceiling || !slices.Equal(got, tc.wantIDs) {
				t.Errorf("violation = %+v, want rule scan.max_severity=%s with %v", v, tc.ceiling, tc.wantIDs)
			}
			if !strings.Contains(body.Message, tc.wantIDs[0]) {
				t.Errorf("message %q does not name %s", body.Message, tc.wantIDs[0])
			}
			if body.Missing != nil {
				t.Errorf("missing = %v, want none for a severity refusal", body.Missing)
			}
			if strings.Contains(body.Message, "requires evidence") {
				t.Errorf("message %q calls a severity failure missing evidence", body.Message)
			}
		})
	}
}

func TestPromote_ScanGateAllowsFindingsAtCeiling(t *testing.T) {
	tests := []struct {
		name     string
		ceiling  string
		findings []lifecycle.Finding
	}{
		{"NoFindings", "low", nil},
		{"HighAtHigh", "high", []lifecycle.Finding{{ID: "CVE-2026-2222", Severity: "HIGH"}, {ID: "CVE-2026-3333", Severity: "LOW"}}},
		{"MediumAtMedium", "medium", []lifecycle.Finding{{ID: "CVE-2026-3333", Severity: "MEDIUM"}}},
		{"LowAtLow", "low", []lifecycle.Finding{{ID: "CVE-2026-4444", Severity: "LOW"}}},
		{"CriticalAtCritical", "critical", []lifecycle.Finding{{ID: "CVE-2026-1111", Severity: "CRITICAL"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newGateEnv(t, tc.ceiling)
			env.sindriScan(t, "", tc.findings...)
			env.mustPromote(t)
		})
	}
}

func TestPromote_ScanGateRefusesWithoutSindriScan(t *testing.T) {
	env := newGateEnv(t, "critical")
	body := env.refusal(t)
	if len(body.Violations) != 1 || !strings.Contains(body.Violations[0].Reason, "no Sindri scan for "+env.image) {
		t.Fatalf("violations = %+v, want no Sindri scan for %s", body.Violations, env.image)
	}
	if !strings.Contains(body.Message, "no Sindri scan") {
		t.Errorf("message = %q", body.Message)
	}
}

func TestPromote_ScanGateIgnoresClientAttachedScan(t *testing.T) {
	env := newGateEnv(t, "critical")
	// A client posts a clean scan and tries to claim it is Sindri's.
	res := mustDo(t, env.h, http.MethodPost,
		"/api/v1/subjects/"+env.image+"/evidence?type=scan&metadata=%7B%22producer%22%3A%22sindri%22%2C%22vulnerabilities%22%3A%5B%5D%7D",
		[]byte(`{"Results":[]}`), "application/vnd.aquasec.trivy.report+json")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("attach: %d %s", res.StatusCode, res.Body)
	}
	body := env.refusal(t)
	if len(body.Violations) != 1 || !strings.Contains(body.Violations[0].Reason, "no Sindri scan") {
		t.Fatalf("violations = %+v, want a refusal for no Sindri scan", body.Violations)
	}

	// The same attachment still satisfies an evidence-presence rule (ADR 16).
	env.setStages(t, `{"stages":[{"name":"prod","gates":[{"evidence":"scan"}]}]}`)
	env.mustPromote(t)
}

func TestPromote_ScanGateRefusesUnknownSeverity(t *testing.T) {
	for _, ceiling := range []string{"critical", "high", "medium", "low"} {
		t.Run("Ceiling_"+ceiling, func(t *testing.T) {
			env := newGateEnv(t, ceiling)
			env.sindriScan(t, "", lifecycle.Finding{ID: "CVE-2026-0000", Severity: "UNKNOWN"})
			body := env.refusal(t)
			if len(body.Violations) != 1 || len(body.Violations[0].Findings) != 1 || body.Violations[0].Findings[0].ID != "CVE-2026-0000" {
				t.Fatalf("violations = %+v, want CVE-2026-0000", body.Violations)
			}
		})
	}
}

func TestPromote_ScanGateRefusesScanWithoutVulnerabilityList(t *testing.T) {
	env := newGateEnv(t, "critical")
	env.attachScanMetadata(t, map[string]any{"producer": "sindri", "severity_counts": map[string]int{"CRITICAL": 0}})
	body := env.refusal(t)
	if len(body.Violations) != 1 || !strings.Contains(body.Violations[0].Reason, "scan again") {
		t.Fatalf("violations = %+v, want a re-scan request", body.Violations)
	}
}

func TestPromote_ScanGateChecksEveryPlatformOfIndex(t *testing.T) {
	env := newGateEnv(t, "high")
	env.sindriScan(t, "linux/amd64")
	env.sindriScan(t, "linux/arm64", lifecycle.Finding{ID: "CVE-2026-1111", Severity: "CRITICAL"})

	body := env.refusal(t)
	if len(body.Violations) != 1 || body.Violations[0].Platform != "linux/arm64" || body.Violations[0].Findings[0].ID != "CVE-2026-1111" {
		t.Fatalf("violations = %+v, want only linux/arm64 refused", body.Violations)
	}

	// A newer clean scan for that platform supersedes the failing one.
	env.sindriScan(t, "linux/arm64")
	env.mustPromote(t)
}

func TestPromote_ScanGateReadsNewestScanPerPlatform(t *testing.T) {
	env := newGateEnv(t, "high")
	env.sindriScan(t, "", lifecycle.Finding{ID: "CVE-2026-1111", Severity: "CRITICAL"})
	env.sindriScan(t, "")
	env.mustPromote(t)

	// A worse scan arriving later does not undo the approval (ADR 23)...
	env.sindriScan(t, "", lifecycle.Finding{ID: "CVE-2026-1111", Severity: "CRITICAL"})
	// ...but it does stop the next promotion.
	env.refusal(t)
}

func TestPromote_ScanGateNamesAtMostTenFindingsInMessage(t *testing.T) {
	env := newGateEnv(t, "low")
	var findings []lifecycle.Finding
	for _, id := range []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12"} {
		findings = append(findings, lifecycle.Finding{ID: "CVE-2026-00" + id, Severity: "HIGH"})
	}
	env.sindriScan(t, "", findings...)
	body := env.refusal(t)
	if len(body.Violations[0].Findings) != 12 {
		t.Errorf("violation lists %d findings, want all 12", len(body.Violations[0].Findings))
	}
	if strings.Contains(body.Message, "CVE-2026-0011") || !strings.Contains(body.Message, "and 2 more") {
		t.Errorf("message = %q, want ten ids and 'and 2 more'", body.Message)
	}
}

func TestPromote_RecordsEvaluatedEvidenceIDs(t *testing.T) {
	env := newGateEnv(t, "high")
	env.setStages(t, `{"stages":[
		{"name":"dev","gates":[]},
		{"name":"prod","gates":[{"evidence":"sbom"},{"scan":{"max_severity":"high"}}]}]}`)
	sbom := env.attach(t, env.image, "sbom", "application/spdx+json", `{"spdxVersion":"SPDX-2.3"}`)
	older := env.sindriScan(t, "", lifecycle.Finding{ID: "CVE-2026-1111", Severity: "CRITICAL"})
	newest := env.sindriScan(t, "")
	env.mustStatus(t, http.MethodPost, "/api/v1/subjects/"+env.image+"/promote", `{"repository":"demo","stage":"dev"}`, http.StatusCreated)
	env.mustPromote(t)
	env.mustStatus(t, http.MethodPost, "/api/v1/subjects/"+env.image+"/revoke", `{"repository":"demo","stage":"prod"}`, http.StatusCreated)

	var events struct {
		Items []struct {
			Stage       string   `json:"stage"`
			Action      string   `json:"action"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"items"`
	}
	env.getJSON(t, "/api/v1/subjects/"+env.image+"/events", &events)
	if len(events.Items) != 3 {
		t.Fatalf("events = %+v, want dev approve, prod approve, prod revoke", events.Items)
	}
	if dev := events.Items[0]; dev.Stage != "dev" || len(dev.EvidenceIDs) != 0 {
		t.Errorf("ungated approve = %+v, want no evidence ids", dev)
	}
	prod := events.Items[1]
	if prod.Action != "approve" || prod.Stage != "prod" || !slices.Equal(prod.EvidenceIDs, []string{sbom, newest}) {
		t.Errorf("gated approve = %+v, want evidence ids [%s %s]", prod, sbom, newest)
	}
	if slices.Contains(prod.EvidenceIDs, older) {
		t.Errorf("approve records the superseded scan %s", older)
	}
	if revoke := events.Items[2]; len(revoke.EvidenceIDs) != 0 {
		t.Errorf("revoke = %+v, want no evidence ids", revoke)
	}

	// The audit trail carries them too.
	var audit struct {
		Items []struct {
			Action      string   `json:"action"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"items"`
	}
	env.getJSON(t, "/api/v1/audit?action=approve&stage=prod", &audit)
	if len(audit.Items) != 1 || !slices.Equal(audit.Items[0].EvidenceIDs, []string{sbom, newest}) {
		t.Errorf("audit = %+v, want the approve with its evidence ids", audit.Items)
	}
}

func TestLifecycle_StagesRoundTripBothRuleKinds(t *testing.T) {
	env := newIdentityEnv(t)
	body := `{"stages":[{"name":"prod","gates":[{"evidence":"sbom"},{"scan":{"max_severity":"high"}}]}]}`
	env.setStages(t, body)
	res := env.do(t, http.MethodGet, "/api/v1/lifecycle/stages", "")
	if res.StatusCode != http.StatusOK || strings.ReplaceAll(res.Body, "\n", "") != body {
		t.Fatalf("GET stages = %d %s, want %s", res.StatusCode, res.Body, body)
	}
}

func TestLifecycle_InvalidScanRulesAreRejected(t *testing.T) {
	env := newIdentityEnv(t)
	tests := []struct {
		name string
		gate string
	}{
		{"UnknownSeverity", `{"scan":{"max_severity":"severe"}}`},
		{"UppercaseSeverity", `{"scan":{"max_severity":"HIGH"}}`},
		{"MissingSeverity", `{"scan":{}}`},
		{"UnknownScanKey", `{"scan":{"max_severity":"high","min":"low"}}`},
		{"BothKinds", `{"evidence":"sbom","scan":{"max_severity":"high"}}`},
		{"TwoScanRules", `{"scan":{"max_severity":"high"}},{"scan":{"max_severity":"low"}}`},
		{"NullScan", `{"scan":null}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := env.do(t, http.MethodPut, "/api/v1/lifecycle/stages", `{"stages":[{"name":"prod","gates":[`+tc.gate+`]}]}`)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%d %s, want 400", res.StatusCode, res.Body)
			}
		})
	}
}
