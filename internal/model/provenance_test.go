package model_test

import (
	"errors"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestHeaderProvenance(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    model.Provenance
		wantErr bool
	}{
		{
			name: "NoHeadersRecordsThePath",
			want: model.Provenance{SourceType: model.ProvenanceNPMPublish},
		},
		{
			name:    "ValuesAreTrimmed",
			headers: map[string]string{model.HeaderSourceRef: " git@example.com:a ", model.HeaderBuildRef: "\tci/1 "},
			want:    model.Provenance{SourceType: model.ProvenanceNPMPublish, SourceRef: "git@example.com:a", BuildRef: "ci/1"},
		},
		{
			name:    "NonASCIIUTF8IsAccepted",
			headers: map[string]string{model.HeaderSourceRef: "https://example.com/ü"},
			want:    model.Provenance{SourceType: model.ProvenanceNPMPublish, SourceRef: "https://example.com/ü"},
		},
		{name: "InvalidUTF8SourceRef", headers: map[string]string{model.HeaderSourceRef: "a\xffb"}, wantErr: true},
		{name: "InvalidUTF8BuildRef", headers: map[string]string{model.HeaderBuildRef: "a\xc3"}, wantErr: true},
		{name: "NULSourceRef", headers: map[string]string{model.HeaderSourceRef: "a\x00b"}, wantErr: true},
		{name: "NULBuildRef", headers: map[string]string{model.HeaderBuildRef: "a\x00b"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := model.HeaderProvenance(model.ProvenanceNPMPublish, func(k string) string { return tc.headers[k] })
			if tc.wantErr {
				if !errors.Is(err, model.ErrInvalidProvenance) {
					t.Fatalf("err = %v, want ErrInvalidProvenance", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
