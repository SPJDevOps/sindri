package oci

import (
	"errors"
	"net/http"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestPushProvenance(t *testing.T) {
	annotated := []byte(`{"schemaVersion":2,"annotations":{
		"org.opencontainers.image.source":"https://git.example.com/a",
		"org.opencontainers.image.revision":"abc123"}}`)
	tests := []struct {
		name    string
		body    []byte
		headers map[string]string
		want    model.Provenance
	}{
		{
			name: "AnnotationsOnly",
			body: annotated,
			want: model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/a", BuildRef: "abc123"},
		},
		{
			name:    "HeadersOverrideAnnotations",
			body:    annotated,
			headers: map[string]string{model.HeaderBuildRef: "ci/run/9"},
			want:    model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/a", BuildRef: "ci/run/9"},
		},
		{
			name:    "HeadersOnly",
			body:    []byte(`{"schemaVersion":2}`),
			headers: map[string]string{model.HeaderSourceRef: " https://git.example.com/b "},
			want:    model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/b"},
		},
		{
			name: "NoClaimStillRecordsThePushPath",
			body: []byte(`{"schemaVersion":2}`),
			want: model.Provenance{SourceType: "oci-push"},
		},
		{
			name: "UnparseableBodyIgnoresAnnotations",
			body: []byte(`not json`),
			want: model.Provenance{SourceType: "oci-push"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got, err := pushProvenance(h, tc.body)
			if err != nil {
				t.Fatalf("pushProvenance: %v", err)
			}
			if got != tc.want {
				t.Fatalf("pushProvenance = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPushProvenance_MalformedHeaderIsRefused(t *testing.T) {
	for _, name := range []string{model.HeaderSourceRef, model.HeaderBuildRef} {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			h[name] = []string{"a\x00b"}
			if _, err := pushProvenance(h, []byte(`{}`)); !errors.Is(err, model.ErrInvalidProvenance) {
				t.Fatalf("err = %v, want ErrInvalidProvenance", err)
			}
		})
	}
}
