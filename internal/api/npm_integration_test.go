//go:build integration

package api_test

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// publishNPM places a version the way the adapter does.
func publishNPM(t *testing.T, e *auditEnv, repo, pkg, filename, version, tag string) {
	t.Helper()
	body := []byte(pkg + "@" + version)
	sum := sha256.Sum256(body)
	if _, err := e.reg.Publish(t.Context(), registry.PublishParams{
		Repo: repo, Package: pkg, Version: version, Filename: filename,
		Body:             bytes.NewReader(body),
		Size:             int64(len(body)),
		ExpectDigest:     model.NewDigest(model.AlgorithmSHA256, sum[:]),
		ExclusiveVersion: true,
		DistTag:          tag,
		Actor:            "user:publisher",
		Provenance:       model.Provenance{SourceType: model.ProvenanceNPMPublish},
	}); err != nil {
		t.Fatalf("Publish %s@%s: %v", pkg, version, err)
	}
}

func TestAPI_ScopedNpmVersionCanBeListedAndDeleted(t *testing.T) {
	e := newAuditEnv(t)
	const repo = "npm-hosted"
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: repo, Type: model.RepoTypeHosted, Protocol: model.ProtocolNPM,
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	publishNPM(t, e, repo, "@acme/ui", "ui-1.0.0.tgz", "1.0.0", "latest")
	publishNPM(t, e, repo, "@acme/ui", "ui-1.1.0.tgz", "1.1.0", "next")
	const pkgPath = "/api/v1/repositories/" + repo + "/packages/@acme%2Fui"

	status, body := e.do(t, "admin", http.MethodGet, pkgPath+"/versions")
	if status != http.StatusOK || !strings.Contains(body, `"1.0.0"`) || !strings.Contains(body, `"1.1.0"`) {
		t.Fatalf("list versions = %d %s, want both versions", status, body)
	}
	status, body = e.do(t, "admin", http.MethodGet, pkgPath+"/versions/1.0.0/files/ui-1.0.0.tgz")
	if status != http.StatusOK || body != "@acme/ui@1.0.0" {
		t.Fatalf("get file = %d %q", status, body)
	}

	if status, body := e.do(t, "admin", http.MethodDelete, pkgPath+"/versions/1.0.0/files/ui-1.0.0.tgz"); status != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", status, body)
	}

	status, body = e.do(t, "admin", http.MethodGet, pkgPath+"/versions")
	if status != http.StatusOK || strings.Contains(body, `"1.0.0"`) || !strings.Contains(body, `"1.1.0"`) {
		t.Fatalf("list versions after delete = %d %s, want only 1.1.0", status, body)
	}
	if status, _ := e.do(t, "admin", http.MethodGet, pkgPath+"/versions/1.0.0/files/ui-1.0.0.tgz"); status != http.StatusNotFound {
		t.Fatalf("get of the deleted file = %d, want 404", status)
	}

	// 1.0.0 held latest, being the first publish. The event names the caller of
	// the delete, not the publisher.
	var events int
	if err := e.db.Pool().QueryRow(t.Context(), `
		SELECT count(*) FROM dist_tag_events
		WHERE repository = $1 AND package = $2 AND tag = 'latest'
		  AND from_version = '1.0.0' AND to_version IS NULL AND actor = 'user:admin'`,
		repo, "@acme/ui").Scan(&events); err != nil {
		t.Fatalf("reading dist_tag_events: %v", err)
	}
	if events != 1 {
		t.Fatalf("removal events by the caller = %d, want 1", events)
	}
}

// The name segment is decoded for every protocol, so a name with no escape in it
// has to reach the other protocols' folding exactly as it did before.
func TestAPI_FoldedNamesAreUnchangedByDecoding(t *testing.T) {
	for _, protocol := range []string{"oci", "pypi"} {
		t.Run(protocol, func(t *testing.T) {
			e := newAuditEnv(t)
			repo := "folded-" + protocol
			if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
				Name: repo, Type: model.RepoTypeHosted, Protocol: model.Protocol(protocol),
			}); err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			base := "/api/v1/repositories/" + repo + "/packages/"
			req := func(method, path string) (int, string) { return e.do(t, "admin", method, base+path) }

			if status, body := req(http.MethodPut, "Foo.Bar/versions/1.0.0/files/a.txt"); status != http.StatusCreated {
				t.Fatalf("PUT = %d %s", status, body)
			}
			if status, body := req(http.MethodGet, "foo-bar/versions/1.0.0/files/a.txt"); status != http.StatusOK {
				t.Fatalf("GET under the folded name = %d %s", status, body)
			}
			if status, body := req(http.MethodGet, "foo_bar/versions"); status != http.StatusOK || !strings.Contains(body, `"1.0.0"`) {
				t.Fatalf("list under another spelling = %d %s", status, body)
			}
			if status, body := req(http.MethodDelete, "FOO.BAR/versions/1.0.0/files/a.txt"); status != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s", status, body)
			}
		})
	}
}

// A name whose last file is deleted stays claimed, but to a reader it is gone:
// the package list omits it and its versions answer 404, as they did before the
// row was kept (SIN-49).
func TestAPI_EmptiedPackageReadsAsAbsent(t *testing.T) {
	e := newAuditEnv(t)
	const repo = "npm-hosted"
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: repo, Type: model.RepoTypeHosted, Protocol: model.ProtocolNPM,
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	publishNPM(t, e, repo, "foo", "foo-1.0.0.tgz", "1.0.0", "latest")
	publishNPM(t, e, repo, "bar", "bar-1.0.0.tgz", "1.0.0", "latest")
	const base = "/api/v1/repositories/" + repo + "/packages"

	if status, body := e.do(t, "admin", http.MethodDelete, base+"/foo/versions/1.0.0/files/foo-1.0.0.tgz"); status != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", status, body)
	}

	status, body := e.do(t, "admin", http.MethodGet, base)
	if status != http.StatusOK || strings.Contains(body, `"foo"`) || !strings.Contains(body, `"bar"`) {
		t.Fatalf("list packages = %d %s, want bar alone", status, body)
	}
	if status, body := e.do(t, "admin", http.MethodGet, base+"/foo/versions"); status != http.StatusNotFound {
		t.Fatalf("versions of the emptied name = %d %s, want 404", status, body)
	}
	claimed, err := e.reg.PackageClaimed(t.Context(), repo, "foo")
	if err != nil || !claimed {
		t.Fatalf("PackageClaimed = %v, %v; want the name still claimed", claimed, err)
	}
}
