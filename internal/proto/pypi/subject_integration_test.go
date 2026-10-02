//go:build integration

package pypi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spj/sindri/internal/model"
)

// TestSubjects_PyPIUploadRecordsOwnerAndProvenance: a twine upload into a
// hosted repository makes the file's digest a subject owned by that
// repository, carrying the provenance the publisher stated in headers.
func TestSubjects_PyPIUploadRecordsOwnerAndProvenance(t *testing.T) {
	client := newTestHandler(t)
	createRepo(t, client, "internal", "pypi")

	wheel := buildWheel(t, "traced", "1.0.0", ">=3.9")
	sum := sha256.Sum256(wheel)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	upload := doMultipartWithHeaders(t, client, "/pypi/internal/upload", map[string]string{
		":action": "file_upload",
		"name":    "traced",
		"version": "1.0.0",
	}, "traced-1.0.0-py3-none-any.whl", wheel, true, map[string]string{
		model.HeaderSourceRef: "https://git.example.com/team/traced",
		model.HeaderBuildRef:  "ci/run/42",
	})
	if upload.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %s", upload.StatusCode, upload.Body)
	}

	res := mustDo(t, client, http.MethodGet, "/api/v1/subjects/"+digest, nil, "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET subject: %d %s", res.StatusCode, res.Body)
	}
	var subject struct {
		Kind       string   `json:"kind"`
		Owners     []string `json:"owners"`
		Provenance []struct {
			SourceType string `json:"source_type"`
			SourceRef  string `json:"source_ref"`
			BuildRef   string `json:"build_ref"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(res.Body), &subject); err != nil {
		t.Fatalf("decoding subject: %v", err)
	}
	if subject.Kind != "pypi-dist" {
		t.Errorf("kind = %q, want pypi-dist", subject.Kind)
	}
	if len(subject.Owners) != 1 || subject.Owners[0] != "repository:internal" {
		t.Errorf("owners = %v, want [repository:internal]", subject.Owners)
	}
	if len(subject.Provenance) != 1 {
		t.Fatalf("provenance = %+v, want one claim", subject.Provenance)
	}
	p := subject.Provenance[0]
	if p.SourceType != "pypi-upload" || p.SourceRef != "https://git.example.com/team/traced" || p.BuildRef != "ci/run/42" {
		t.Errorf("provenance = %+v", p)
	}
}
