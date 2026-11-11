package lifecycle

import (
	"errors"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestParseGateRule(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantEvidence model.EvidenceType
		wantSeverity Severity
		wantErr      bool
	}{
		{name: "EvidenceType", raw: `{"evidence":"sbom"}`, wantEvidence: model.EvidenceSBOM},
		{name: "Signature", raw: `{"evidence":"signature"}`, wantEvidence: model.EvidenceSignature},
		{name: "ScanCeilingHigh", raw: `{"scan":{"max_severity":"high"}}`, wantSeverity: SeverityHigh},
		{name: "ScanCeilingCritical", raw: `{"scan":{"max_severity":"critical"}}`, wantSeverity: SeverityCritical},
		{name: "ScanCeilingMedium", raw: `{"scan":{"max_severity":"medium"}}`, wantSeverity: SeverityMedium},
		{name: "ScanCeilingLow", raw: `{"scan":{"max_severity":"low"}}`, wantSeverity: SeverityLow},
		{name: "UnknownType", raw: `{"evidence":"vibes"}`, wantErr: true},
		{name: "MissingType", raw: `{}`, wantErr: true},
		{name: "BothKindsSet", raw: `{"evidence":"sbom","scan":{"max_severity":"high"}}`, wantErr: true},
		{name: "NeitherKindSetNullScan", raw: `{"scan":null}`, wantErr: true},
		{name: "ScanWithoutSeverity", raw: `{"scan":{}}`, wantErr: true},
		{name: "UnknownSeverity", raw: `{"scan":{"max_severity":"severe"}}`, wantErr: true},
		{name: "UppercaseSeverityIsUnknown", raw: `{"scan":{"max_severity":"HIGH"}}`, wantErr: true},
		{name: "UnknownSeverityIsNotACeiling", raw: `{"scan":{"max_severity":"unknown"}}`, wantErr: true},
		{name: "UnknownKeyIsNotIgnored", raw: `{"evidence":"sbom","stage":"integration"}`, wantErr: true},
		{name: "NestedUnknownKeyIsNotIgnored", raw: `{"scan":{"max_severity":"high","min_severity":"low"}}`, wantErr: true},
		{name: "TrailingData", raw: `{"evidence":"sbom"}{"evidence":"scan"}`, wantErr: true},
		{name: "NotJSON", raw: `sbom`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := ParseGateRule([]byte(tc.raw))
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("err = %v, want ErrInvalidInput", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGateRule: %v", err)
			}
			if rule.Evidence != tc.wantEvidence {
				t.Fatalf("evidence = %q, want %q", rule.Evidence, tc.wantEvidence)
			}
			if tc.wantSeverity != "" && (rule.Scan == nil || rule.Scan.MaxSeverity != tc.wantSeverity) {
				t.Fatalf("scan = %+v, want max_severity %q", rule.Scan, tc.wantSeverity)
			}
			if tc.wantSeverity == "" && rule.Scan != nil {
				t.Fatalf("scan = %+v, want none", rule.Scan)
			}
		})
	}
}

func TestMarshalRule_RoundTripsBothKinds(t *testing.T) {
	for _, raw := range []string{`{"evidence":"sbom"}`, `{"scan":{"max_severity":"medium"}}`} {
		t.Run(raw, func(t *testing.T) {
			rule, err := ParseGateRule([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalRule(rule)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != raw {
				t.Fatalf("marshalRule = %s, want %s", got, raw)
			}
		})
	}
}

func TestSeverityRefuses(t *testing.T) {
	tests := []struct {
		ceiling Severity
		finding string
		want    bool
	}{
		{SeverityCritical, "CRITICAL", false},
		{SeverityCritical, "UNKNOWN", true},
		{SeverityHigh, "CRITICAL", true},
		{SeverityHigh, "HIGH", false},
		{SeverityHigh, "UNKNOWN", true},
		{SeverityMedium, "HIGH", true},
		{SeverityMedium, "MEDIUM", false},
		{SeverityLow, "MEDIUM", true},
		{SeverityLow, "LOW", false},
		{SeverityLow, "UNKNOWN", true},
		{SeverityLow, "SOMETHING_NEW", true},
		{SeverityCritical, "", true},
	}
	for _, tc := range tests {
		t.Run(string(tc.ceiling)+"_"+tc.finding, func(t *testing.T) {
			if got := tc.ceiling.Refuses(tc.finding); got != tc.want {
				t.Fatalf("%s.Refuses(%q) = %v, want %v", tc.ceiling, tc.finding, got, tc.want)
			}
		})
	}
}

func TestValidateStages(t *testing.T) {
	gate := func(t model.EvidenceType) GateRule { return GateRule{Evidence: t} }
	scanGate := func(s Severity) GateRule { return GateRule{Scan: &ScanRule{MaxSeverity: s}} }
	tests := []struct {
		name    string
		specs   []StageSpec
		wantErr bool
	}{
		{name: "Empty", specs: nil},
		{name: "OrderedWithGates", specs: []StageSpec{
			{Name: "dev"},
			{Name: "integration", Gates: []GateRule{gate(model.EvidenceSBOM)}},
			{Name: "prod", Gates: []GateRule{gate(model.EvidenceSBOM), gate(model.EvidenceSignature)}},
		}},
		{name: "DuplicateStage", specs: []StageSpec{{Name: "dev"}, {Name: "dev"}}, wantErr: true},
		{name: "DuplicateGate", specs: []StageSpec{{Name: "prod", Gates: []GateRule{gate(model.EvidenceSBOM), gate(model.EvidenceSBOM)}}}, wantErr: true},
		{name: "ScanRuleBesideEvidence", specs: []StageSpec{{Name: "prod", Gates: []GateRule{gate(model.EvidenceSBOM), scanGate(SeverityHigh)}}}},
		{name: "TwoScanRulesInOneStage", specs: []StageSpec{{Name: "prod", Gates: []GateRule{scanGate(SeverityHigh), scanGate(SeverityLow)}}}, wantErr: true},
		{name: "ScanRuleWithUnknownSeverity", specs: []StageSpec{{Name: "prod", Gates: []GateRule{scanGate("severe")}}}, wantErr: true},
		{name: "ScanRuleInTwoStages", specs: []StageSpec{{Name: "a", Gates: []GateRule{scanGate(SeverityHigh)}}, {Name: "b", Gates: []GateRule{scanGate(SeverityHigh)}}}},
		{name: "UnknownEvidence", specs: []StageSpec{{Name: "prod", Gates: []GateRule{gate("vibes")}}}, wantErr: true},
		{name: "EmptyName", specs: []StageSpec{{Name: ""}}, wantErr: true},
		{name: "Uppercase", specs: []StageSpec{{Name: "Prod"}}, wantErr: true},
		{name: "LeadingSeparator", specs: []StageSpec{{Name: "-prod"}}, wantErr: true},
		{name: "Separators", specs: []StageSpec{{Name: "pre-prod.eu_1"}}},
		{name: "TooLong", specs: []StageSpec{{Name: "a234567890123456789012345678901234567890123456789012345678901234"}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStages(tc.specs)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestGateError_MatchesErrGateFailed(t *testing.T) {
	var err error = &GateError{Stage: "prod", Missing: []string{"signature"}}
	if !errors.Is(err, ErrGateFailed) {
		t.Fatal("errors.Is(GateError, ErrGateFailed) = false")
	}
	var gate *GateError
	if !errors.As(err, &gate) || gate.Missing[0] != "signature" {
		t.Fatalf("errors.As = %+v", gate)
	}
}
