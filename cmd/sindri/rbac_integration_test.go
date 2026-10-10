//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/testsupport"
)

// rbacRepos is the desired state the RBAC tests run against: the same shape
// as the gate's testdata/config/sindri.yaml.
const rbacRepos = `
# --- desired
repos:
  docker:
    protocol: oci
    read: [team:payments, team:platform]
    write: [team:platform]
  internal-py:
    protocol: pypi
    read: [team:payments, team:platform]
    write: [team:platform]
  secret:
    protocol: oci
    read: [team:security]
    write: [team:security]
`

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushImage pushes a one-layer image and returns the status of the first
// request that did not succeed, or of the manifest PUT when all did.
func (s *stack) pushImage(name, tag, seed string, cred func(*http.Request)) (int, string) {
	s.t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("layer for " + seed)
	for _, blob := range [][]byte{config, layer} {
		r := s.request(http.MethodPost, "/v2/"+name+"/blobs/uploads/?digest="+sha(blob), bytes.NewReader(blob), cred)
		if r.status != http.StatusCreated {
			return r.status, string(r.body)
		}
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(sha(config)), Size: int64(len(config))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.Digest(sha(layer)), Size: int64(len(layer))}},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	r := s.request(http.MethodPut, "/v2/"+name+"/manifests/"+tag, bytes.NewReader(manifest), func(req *http.Request) {
		req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
		if cred != nil {
			cred(req)
		}
	})
	return r.status, string(r.body)
}

func basic(user, password string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, password) }
}

func ociCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil || len(e.Errors) == 0 {
		t.Fatalf("not an OCI error body: %s", body)
	}
	return e.Errors[0].Code
}

// Gate test 8: an anonymous docker pull and an anonymous pip install are
// both refused, with the challenge each client follows, on every route they
// use. So is a wrong credential.
func TestRBAC_AnonymousPullAndInstallRefused(t *testing.T) {
	s := startServe(t, testsupport.StartPostgres(t), rbacRepos, nil)
	if status, body := s.pushImage("docker/demo/app", "1.0", "anon", bearer(bootstrapToken)); status != http.StatusCreated {
		t.Fatalf("setup push: %d %s", status, body)
	}
	manifest := s.request(http.MethodGet, "/v2/docker/demo/app/manifests/1.0", nil, bearer(bootstrapToken))
	if manifest.status != http.StatusOK {
		t.Fatalf("setup pull: %d", manifest.status)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(manifest.body, &m); err != nil {
		t.Fatal(err)
	}

	paths := []struct {
		name, path, challenge string
	}{
		{"OCIBase", "/v2/", "Basic"},
		{"OCIManifest", "/v2/docker/demo/app/manifests/1.0", "Basic"},
		{"OCIBlob", "/v2/docker/demo/app/blobs/" + m.Layers[0].Digest.String(), "Basic"},
		{"OCITags", "/v2/docker/demo/app/tags/list", "Basic"},
		{"PyPIRootIndex", "/pypi/internal-py/simple/", "Basic"},
		{"PyPIProjectIndex", "/pypi/internal-py/simple/demo-package/", "Basic"},
		{"PyPIDownload", "/pypi/internal-py/packages/demo-package/demo_package-1.0-py3-none-any.whl", "Basic"},
		{"ManagementAPI", "/api/v1/repositories", "Bearer"},
		{"Audit", "/api/v1/audit", "Bearer"},
		{"GCCandidates", "/api/v1/gc/candidates", "Bearer"},
		{"Job", "/api/v1/jobs/1", "Bearer"},
	}
	creds := []struct {
		name string
		set  func(*http.Request)
	}{
		{"Anonymous", nil},
		{"WrongBearer", bearer("not-the-token")},
		{"WrongBasic", basic("someone", "not-the-token")},
		{"UnknownMachineToken", basic("ci", "sindri_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")},
	}
	for _, p := range paths {
		for _, c := range creds {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				r := s.request(http.MethodGet, p.path, nil, c.set)
				if r.status != http.StatusUnauthorized {
					t.Fatalf("status %d, want 401: %s", r.status, r.body)
				}
				if got := r.header.Get("WWW-Authenticate"); !strings.HasPrefix(got, p.challenge) {
					t.Fatalf("challenge %q, want %s", got, p.challenge)
				}
				if strings.HasPrefix(p.path, "/v2/") {
					if code := ociCode(t, r.body); code != "UNAUTHORIZED" {
						t.Fatalf("OCI code %q, want UNAUTHORIZED", code)
					}
				}
			})
		}
	}

	// Liveness, readiness, and metrics stay open: an orchestrator has no
	// credential, and they disclose nothing about content.
	for _, open := range []string{"/healthz", "/readyz", "/metrics"} {
		if r := s.request(http.MethodGet, open, nil, nil); r.status != http.StatusOK {
			t.Errorf("%s: %d, want 200 without credentials", open, r.status)
		}
	}
}

type issued struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

func (s *stack) issueToken(body string) issued {
	s.t.Helper()
	r := s.request(http.MethodPost, "/api/v1/tokens", strings.NewReader(body), bearer(bootstrapToken))
	if r.status != http.StatusCreated {
		s.t.Fatalf("issue token %s: %d %s", body, r.status, r.body)
	}
	var tok issued
	if err := json.Unmarshal(r.body, &tok); err != nil {
		s.t.Fatal(err)
	}
	return tok
}

// A machine token for CI is scoped to one repository and one role: it can do
// what the role allows there and nothing anywhere else, and revoking it ends
// it at once.
func TestRBAC_MachineTokenScopedToRepository(t *testing.T) {
	s := startServe(t, testsupport.StartPostgres(t), rbacRepos, nil)
	if status, body := s.pushImage("docker/demo/app", "1.0", "tok", bearer(bootstrapToken)); status != http.StatusCreated {
		t.Fatalf("setup push: %d %s", status, body)
	}
	if status, _ := s.pushImage("secret/app", "1.0", "tok", bearer(bootstrapToken)); status != http.StatusCreated {
		t.Fatalf("setup push to secret: %d", status)
	}

	reader := s.issueToken(`{"name":"ci-pull","repository":"docker","role":"read"}`)
	writer := s.issueToken(`{"name":"ci-push","repository":"docker","role":"write"}`)
	if !strings.HasPrefix(reader.Token, "sindri_") {
		t.Fatalf("token %q lacks the sindri_ prefix secret scanners look for", reader.Token)
	}

	asReader := basic("ci", reader.Token)
	if r := s.request(http.MethodGet, "/v2/docker/demo/app/manifests/1.0", nil, asReader); r.status != http.StatusOK {
		t.Fatalf("read token pull: %d %s", r.status, r.body)
	}
	if status, body := s.pushImage("docker/demo/app", "2.0", "denied", asReader); status != http.StatusForbidden || ociCode(t, []byte(body)) != "DENIED" {
		t.Fatalf("read token push: %d %s, want 403 DENIED", status, body)
	}
	if r := s.request(http.MethodGet, "/v2/secret/app/manifests/1.0", nil, asReader); r.status != http.StatusNotFound {
		t.Fatalf("read token on another repository: %d, want 404 so the name is not disclosed", r.status)
	}
	if status, _ := s.pushImage("brand-new/app", "1.0", "x", basic("ci", writer.Token)); status != http.StatusForbidden {
		t.Fatalf("write token creating a repository by pushing: %d, want 403", status)
	}
	if status, body := s.pushImage("docker/demo/app", "2.0", "allowed", basic("ci", writer.Token)); status != http.StatusCreated {
		t.Fatalf("write token push: %d %s", status, body)
	}
	if r := s.request(http.MethodPost, "/api/v1/repositories", strings.NewReader(`{"name":"x","protocol":"oci"}`), bearer(writer.Token)); r.status != http.StatusForbidden {
		t.Fatalf("write token creating a repository through the API: %d, want 403", r.status)
	}

	// The listing says who issued each token and never shows a secret.
	listing := s.request(http.MethodGet, "/api/v1/tokens", nil, bearer(bootstrapToken))
	if !strings.Contains(string(listing.body), `"created_by":"bootstrap"`) || strings.Contains(string(listing.body), reader.Token) {
		t.Fatalf("token listing: %s", listing.body)
	}

	if r := s.request(http.MethodDelete, "/api/v1/tokens/"+reader.ID, nil, bearer(bootstrapToken)); r.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	if r := s.request(http.MethodGet, "/v2/docker/demo/app/manifests/1.0", nil, asReader); r.status != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", r.status)
	}
}
