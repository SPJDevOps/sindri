//go:build integration

package registry_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// SIN-116: a row written before credentials were refused keeps working. It is
// found by name, so startup can warn, and its diff never prints the secret.
func TestProxiesWithUpstreamCredentials_FindsLegacyRowsByName(t *testing.T) {
	f := newEvictFixture(t)
	for _, name := range []string{"clean", "legacy"} {
		if _, err := f.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
			Name: name, Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI,
			UpstreamURL: "https://registry.example.com", TTLSeconds: 60,
		}); err != nil {
			t.Fatalf("CreateRepository %s: %v", name, err)
		}
	}
	f.exec(`UPDATE remotes SET upstream_url = 'https://alice:s3cret@registry.example.com'
		WHERE repo_id = (SELECT id FROM repositories WHERE name = 'legacy')`)

	got, err := f.reg.ProxiesWithUpstreamCredentials(t.Context())
	if err != nil {
		t.Fatalf("ProxiesWithUpstreamCredentials: %v", err)
	}
	if want := []string{"legacy"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The row still resolves, so existing deployments keep authenticating.
	remote, err := f.reg.GetRemote(t.Context(), "legacy")
	if err != nil || remote.UpstreamURL != "https://alice:s3cret@registry.example.com" {
		t.Fatalf("GetRemote = %+v, %v; the stored URL must be left alone", remote, err)
	}
}

func TestApplyConfig_DiffNeverPrintsUpstreamCredentials(t *testing.T) {
	f := newEvictFixture(t)
	desired := func(url string) []registry.DesiredRepository {
		return []registry.DesiredRepository{{
			Name: "mirror", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI, UpstreamURL: url,
		}}
	}
	opts := registry.ApplyOptions{Source: model.SourceApply, Namespaces: []string{""}}
	if _, err := f.reg.ApplyConfig(t.Context(), f.db.Queries(), desired("https://registry.example.com"), opts); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	f.exec(`UPDATE remotes SET upstream_url = 'https://alice:s3cret@registry.example.com'`)

	changes, err := f.reg.ApplyConfig(t.Context(), f.db.Queries(), desired("https://registry.example.org"), opts)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(changes.Updated) != 1 {
		t.Fatalf("updated = %+v, want one repository", changes.Updated)
	}
	for _, fc := range changes.Updated[0].Fields {
		for _, v := range []string{fc.Before, fc.After} {
			if strings.Contains(v, "s3cret") || strings.Contains(v, "alice") {
				t.Errorf("field %s carries the credential: %q", fc.Field, v)
			}
		}
	}
}
