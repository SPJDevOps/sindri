//go:build integration

package resolve_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func (e *testEnv) createHostedOCI(t *testing.T, name string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI,
	}); err != nil {
		t.Fatalf("create hosted %s: %v", name, err)
	}
}

// pushReferrer pushes a manifest naming subject into repo and returns its digest.
func (e *testEnv) pushReferrer(t *testing.T, repo, subject, seed string) string {
	t.Helper()
	config := []byte(`{}`)
	for _, blob := range [][]byte{config} {
		rec := e.do(t, http.MethodPost, "/v2/"+repo+"/blobs/uploads/?digest=sha256:"+shaHex(blob), blob, "application/octet-stream")
		if rec.Code != http.StatusCreated {
			t.Fatalf("blob upload to %s: %d %s", repo, rec.Code, rec.Body.String())
		}
	}
	body, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example." + seed,
		Config:       ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON, Digest: digest.Digest("sha256:" + shaHex(config)), Size: int64(len(config))},
		Layers:       []ocispec.Descriptor{},
		Subject:      &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(subject), Size: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + shaHex(body)
	rec := e.do(t, http.MethodPut, "/v2/"+repo+"/manifests/"+d, body, ocispec.MediaTypeImageManifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("referrer push to %s: %d %s", repo, rec.Code, rec.Body.String())
	}
	return d
}

func TestReferrers_ProxyIsEmptyAndNeverContactsUpstream(t *testing.T) {
	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	env.createOCIProxy(t, "hub", 3600)

	subject := "sha256:" + shaHex([]byte("subject"))
	before := env.requests.Load()
	page, err := env.resolver.ListOCIReferrers(t.Context(), "hub", mustDigest(t, subject), "", "", 0)
	if err != nil {
		t.Fatalf("ListOCIReferrers on a proxy: %v", err)
	}
	if len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("proxy page = %+v, want empty", page)
	}
	rec := env.do(t, http.MethodGet, "/v2/hub/library/x/referrers/"+subject, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET referrers on a proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := env.requests.Load() - before; got != 0 {
		t.Fatalf("upstream saw %d requests, want 0", got)
	}
}

// A repository of another protocol has no referrers route, whatever its type:
// it answers as the tags and manifest routes do, not with an empty index.
func TestReferrers_RepositoryOfAnotherProtocolIsNotOCI(t *testing.T) {
	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	env.createHostedNPM(t, "js")
	env.createProxyNPM(t, "npmjs", 3600)
	env.createVirtualNPM(t, "javascript", "js", "npmjs")

	subject := mustDigest(t, "sha256:"+shaHex([]byte("subject")))
	for _, name := range []string{"js", "npmjs", "javascript"} {
		t.Run(name, func(t *testing.T) {
			if _, err := env.resolver.ListOCIReferrers(t.Context(), name, subject, "", "", 0); !errors.Is(err, registry.ErrWrongProtocol) {
				t.Fatalf("ListOCIReferrers = %v, want ErrWrongProtocol", err)
			}
		})
	}
}

func TestReferrers_VirtualAnswersFromFirstReadableHostedMember(t *testing.T) {
	img := newTestImage(t, []byte("layer"), nil)

	cases := []struct {
		name string
		// setup returns the virtual's members in order and the digests it
		// expects listed.
		setup    func(t *testing.T, env *testEnv) (members []string, want []string)
		deniedBy string
	}{
		{
			name: "first hosted member that places the subject answers",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createHostedOCI(t, "m1")
				env.createHostedOCI(t, "m2")
				env.pushImage(t, "m1", "1", img)
				env.pushImage(t, "m2", "1", img)
				a := env.pushReferrer(t, "m1", img.manifestDigest, "one")
				env.pushReferrer(t, "m2", img.manifestDigest, "two")
				return []string{"m1", "m2"}, []string{a}
			},
		},
		{
			name: "a member that places the subject answers even when its list is empty",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createHostedOCI(t, "m1")
				env.createHostedOCI(t, "m2")
				env.pushImage(t, "m1", "1", img)
				env.pushImage(t, "m2", "1", img)
				env.pushReferrer(t, "m2", img.manifestDigest, "two")
				return []string{"m1", "m2"}, nil
			},
		},
		{
			name: "a member that does not place the subject is passed over",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createHostedOCI(t, "m1")
				env.createHostedOCI(t, "m2")
				env.pushImage(t, "m2", "1", img)
				b := env.pushReferrer(t, "m2", img.manifestDigest, "two")
				return []string{"m1", "m2"}, []string{b}
			},
		},
		{
			name:     "an unreadable first member is passed over",
			deniedBy: "m1",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createHostedOCI(t, "m1")
				env.createHostedOCI(t, "m2")
				env.pushImage(t, "m1", "1", img)
				env.pushImage(t, "m2", "1", img)
				env.pushReferrer(t, "m1", img.manifestDigest, "one")
				b := env.pushReferrer(t, "m2", img.manifestDigest, "two")
				return []string{"m1", "m2"}, []string{b}
			},
		},
		{
			name: "only a proxy member answers empty",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createOCIProxy(t, "hub", 3600)
				return []string{"hub"}, nil
			},
		},
		{
			name: "a nested virtual answers from its first hosted member",
			setup: func(t *testing.T, env *testEnv) ([]string, []string) {
				env.createHostedOCI(t, "m1")
				env.pushImage(t, "m1", "1", img)
				a := env.pushReferrer(t, "m1", img.manifestDigest, "one")
				env.createVirtualOf(t, "inner", model.ProtocolOCI, "m1")
				return []string{"inner"}, []string{a}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
			if tc.deniedBy != "" {
				env.resolver.SetMemberAccess(func(_ context.Context, m model.Repository) bool { return m.Name != tc.deniedBy })
			}
			members, want := tc.setup(t, env)
			env.createVirtualOf(t, "all", model.ProtocolOCI, members...)

			page, err := env.resolver.ListOCIReferrers(t.Context(), "all", mustDigest(t, img.manifestDigest), "", "", 0)
			if err != nil {
				t.Fatalf("ListOCIReferrers: %v", err)
			}
			got := make([]string, 0, len(page.Items))
			for _, r := range page.Items {
				got = append(got, r.Digest.String())
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("listed %v, want %v", got, want)
			}
		})
	}
}

func mustDigest(t *testing.T, s string) model.Digest {
	t.Helper()
	d, err := model.ParseDigest(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
