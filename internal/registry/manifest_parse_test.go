package registry

import (
	"errors"
	"testing"

	"github.com/spj/sindri/internal/model"
)

const (
	testSubject = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testConfig  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseManifest_SubjectArtifactTypeAndAnnotations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		mediaType    string
		body         string
		wantErr      bool
		wantSubject  string
		wantArtifact string
		wantNotes    map[string]string
		wantChildren int
	}{
		{
			name:         "image manifest without subject",
			mediaType:    "application/vnd.oci.image.manifest.v1+json",
			body:         `{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + testConfig + `","size":2},"layers":[]}`,
			wantArtifact: "application/vnd.oci.image.config.v1+json",
			wantChildren: 1,
		},
		{
			name:         "explicit artifactType wins over config media type",
			mediaType:    "application/vnd.oci.image.manifest.v1+json",
			body:         `{"schemaVersion":2,"artifactType":"application/vnd.example.sig","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + testConfig + `","size":2},"layers":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + testSubject + `","size":1},"annotations":{"k":"v"}}`,
			wantSubject:  testSubject,
			wantArtifact: "application/vnd.example.sig",
			wantNotes:    map[string]string{"k": "v"},
			wantChildren: 1,
		},
		{
			name:         "empty artifactType falls back to config media type",
			mediaType:    "application/vnd.oci.image.manifest.v1+json",
			body:         `{"schemaVersion":2,"config":{"mediaType":"application/vnd.example.cfg","digest":"` + testConfig + `","size":2},"layers":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + testSubject + `","size":1}}`,
			wantSubject:  testSubject,
			wantArtifact: "application/vnd.example.cfg",
			wantChildren: 1,
		},
		{
			name:         "index with subject uses only its own artifactType",
			mediaType:    "application/vnd.oci.image.index.v1+json",
			body:         `{"schemaVersion":2,"manifests":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + testSubject + `","size":1}}`,
			wantSubject:  testSubject,
			wantArtifact: "",
		},
		{
			name:      "malformed subject digest is refused",
			mediaType: "application/vnd.oci.image.manifest.v1+json",
			body:      `{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + testConfig + `","size":2},"layers":[],"subject":{"mediaType":"x","digest":"sha256:nothex","size":1}}`,
			wantErr:   true,
		},
		{
			name:      "malformed subject digest on an index is refused",
			mediaType: "application/vnd.oci.image.index.v1+json",
			body:      `{"schemaVersion":2,"manifests":[],"subject":{"mediaType":"x","digest":"nonsense","size":1}}`,
			wantErr:   true,
		},
		{
			name:      "unknown media type carries no subject",
			mediaType: "application/vnd.example+json",
			body:      `{"subject":{"digest":"` + testSubject + `"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseManifest(tc.mediaType, []byte(tc.body))
			if tc.wantErr {
				if err == nil || !errors.Is(err, model.ErrInvalidDigest) {
					t.Fatalf("err = %v, want ErrInvalidDigest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseManifest: %v", err)
			}
			if got.hasSubject != (tc.wantSubject != "") || (tc.wantSubject != "" && got.subject.String() != tc.wantSubject) {
				t.Fatalf("subject = %v (%v), want %q", got.subject, got.hasSubject, tc.wantSubject)
			}
			if got.artifactType != tc.wantArtifact {
				t.Fatalf("artifactType = %q, want %q", got.artifactType, tc.wantArtifact)
			}
			if len(got.children) != tc.wantChildren {
				t.Fatalf("children = %d, want %d", len(got.children), tc.wantChildren)
			}
			for k, v := range tc.wantNotes {
				if got.annotations[k] != v {
					t.Fatalf("annotations = %v, want %v", got.annotations, tc.wantNotes)
				}
			}
		})
	}
}
