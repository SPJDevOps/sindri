package lifecycle

import (
	"errors"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestParseGateRule(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    model.EvidenceType
		wantErr bool
	}{
		{name: "EvidenceType", raw: `{"evidence":"sbom"}`, want: model.EvidenceSBOM},
		{name: "Signature", raw: `{"evidence":"signature"}`, want: model.EvidenceSignature},
		{name: "UnknownType", raw: `{"evidence":"vibes"}`, wantErr: true},
		{name: "MissingType", raw: `{}`, wantErr: true},
		{name: "UnknownKeyIsNotIgnored", raw: `{"evidence":"sbom","stage":"integration"}`, wantErr: true},
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
			if rule.Evidence != tc.want {
				t.Fatalf("evidence = %q, want %q", rule.Evidence, tc.want)
			}
		})
	}
}

func TestValidateStages(t *testing.T) {
	gate := func(t model.EvidenceType) GateRule { return GateRule{Evidence: t} }
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
