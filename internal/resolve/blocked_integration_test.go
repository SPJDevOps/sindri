//go:build integration

package resolve_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

type blockedBody struct {
	Digest    string `json:"digest"`
	BlockedBy string `json:"blocked_by"`
	Reason    string `json:"reason"`
	BlockedAt string `json:"blocked_at"`
}

func (e *testEnv) block(t *testing.T, repo, digest, body string) (int, blockedBody) {
	t.Helper()
	rec := e.do(t, http.MethodPut, "/api/v1/repositories/"+repo+"/blocked/"+digest, []byte(body), "application/json")
	var out blockedBody
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *testEnv) pull(t *testing.T, repo, kind, ref string) int {
	t.Helper()
	return e.do(t, http.MethodGet, "/v2/"+repo+"/library/x/"+kind+"/"+ref, nil, "").Code
}

func TestProxyOCI_EvictedDigestIsRefusedAndNeverRefetched(t *testing.T) {
	stub := &ociStub{}
	img := newTestImage(t, []byte("layer"), nil)
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	env.createOCIProxy(t, "other", 3600)

	for _, repo := range []string{"hub", "other"} {
		if code := env.pull(t, repo, "manifests", "latest"); code != http.StatusOK {
			t.Fatalf("%s manifest: %d", repo, code)
		}
		if code := env.pull(t, repo, "blobs", img.layerDigest); code != http.StatusOK {
			t.Fatalf("%s layer: %d", repo, code)
		}
	}

	code, got := env.block(t, "hub", img.manifestDigest, `{"reason":"CVE-2026-0001"}`)
	if code != http.StatusOK || got.Digest != img.manifestDigest || got.Reason != "CVE-2026-0001" || got.BlockedBy != "bootstrap" || got.BlockedAt == "" {
		t.Fatalf("block: %d %+v", code, got)
	}

	upstream := func() int { return int(env.requests.Load()) }
	before := upstream()
	if c := env.pull(t, "hub", "manifests", img.manifestDigest); c != http.StatusNotFound {
		t.Errorf("by digest after eviction: %d, want 404", c)
	}
	if n := upstream() - before; n != 0 {
		t.Errorf("by-digest pull of the blocked manifest made %d upstream requests, want 0", n)
	}
	// The tag asks upstream once, for its digest, and is refused before any
	// placement is recorded.
	if c := env.pull(t, "hub", "manifests", "latest"); c != http.StatusNotFound {
		t.Errorf("by tag after eviction: %d, want 404", c)
	}
	manifestDigest, err := model.ParseDigest(img.manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.reg.CheckPlacement(t.Context(), "hub", manifestDigest); err == nil {
		t.Error("a tag fetch re-placed the blocked manifest")
	}
	// The layer is not blocked, only unplaced with its manifest: a pull
	// fetches it from upstream again, exactly once, and succeeds.
	before = upstream()
	if c := env.pull(t, "hub", "blobs", img.layerDigest); c != http.StatusOK {
		t.Errorf("unblocked layer after eviction: %d, want 200", c)
	}
	if n := upstream() - before; n != 1 {
		t.Errorf("layer refetch made %d upstream requests, want 1", n)
	}

	// The same digest in another repository is unaffected (I2).
	if c := env.pull(t, "other", "manifests", img.manifestDigest); c != http.StatusOK {
		t.Errorf("other repository by digest: %d, want 200", c)
	}

	// Blocking is idempotent and keeps the first record.
	code2, again := env.block(t, "hub", img.manifestDigest, `{"reason":"second"}`)
	if code2 != http.StatusOK || again.Reason != "CVE-2026-0001" || again.BlockedBy != got.BlockedBy {
		t.Errorf("second block: %d %+v", code2, again)
	}

	// The list shows the audit record.
	rec := env.do(t, http.MethodGet, "/api/v1/repositories/hub/blocked", nil, "")
	var list struct{ Items []blockedBody }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].Digest != img.manifestDigest {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Unblocking allows a refetch.
	if rec := env.do(t, http.MethodDelete, "/api/v1/repositories/hub/blocked/"+img.manifestDigest, nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unblock: %d", rec.Code)
	}
	if c := env.pull(t, "hub", "manifests", img.manifestDigest); c != http.StatusOK {
		t.Errorf("after unblock: %d, want 200", c)
	}
	if rec := env.do(t, http.MethodDelete, "/api/v1/repositories/hub/blocked/"+img.manifestDigest, nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unblock twice: %d, want 404", rec.Code)
	}
}

func TestProxyOCI_BlockedDigestNeverReachesUpstream(t *testing.T) {
	stub := &ociStub{}
	img := newTestImage(t, []byte("layer"), nil)
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)

	// Pre-emptive: nothing is placed, and the block still holds.
	if code, _ := env.block(t, "hub", img.layerDigest, ""); code != http.StatusOK {
		t.Fatalf("pre-emptive block: %d", code)
	}
	if code, _ := env.block(t, "hub", img.manifestDigest, ""); code != http.StatusOK {
		t.Fatalf("pre-emptive block: %d", code)
	}
	before := env.requests.Load()
	if c := env.pull(t, "hub", "manifests", img.manifestDigest); c != http.StatusNotFound {
		t.Errorf("manifest by digest: %d, want 404", c)
	}
	if c := env.pull(t, "hub", "blobs", img.layerDigest); c != http.StatusNotFound {
		t.Errorf("blob by digest: %d, want 404", c)
	}
	rec := env.do(t, http.MethodHead, "/v2/hub/library/x/blobs/"+img.layerDigest, nil, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("blob HEAD: %d, want 404", rec.Code)
	}
	if n := env.requests.Load() - before; n != 0 {
		t.Errorf("%d upstream requests for blocked digests, want 0", n)
	}
	if c := env.pull(t, "hub", "manifests", "latest"); c != http.StatusNotFound {
		t.Errorf("tag resolving to a blocked digest: %d, want 404", c)
	}
	if body := env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/"+img.manifestDigest, nil, "").Body.String(); !strings.Contains(body, "MANIFEST_UNKNOWN") {
		t.Errorf("error body: %s", body)
	}
}

func TestProxyOCI_EvictUnlinksChildrenNothingElseReferences(t *testing.T) {
	stub := &ociStub{}
	a := newTestImage(t, []byte("layer-a"), map[string]string{"n": "a"})
	stub.set(a)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	if c := env.pull(t, "hub", "manifests", "a"); c != http.StatusOK {
		t.Fatalf("pull a: %d", c)
	}
	if c := env.pull(t, "hub", "blobs", a.layerDigest); c != http.StatusOK {
		t.Fatalf("pull layer: %d", c)
	}
	if c := env.pull(t, "hub", "blobs", a.configDigest); c != http.StatusOK {
		t.Fatalf("pull config: %d", c)
	}
	// A second image sharing the config keeps it placed.
	b := newTestImage(t, []byte("layer-b"), map[string]string{"n": "b"})
	stub.set(b)
	if c := env.pull(t, "hub", "manifests", "b"); c != http.StatusOK {
		t.Fatalf("pull b: %d", c)
	}

	if code, _ := env.block(t, "hub", a.manifestDigest, ""); code != http.StatusOK {
		t.Fatalf("block: %d", code)
	}
	hub, err := env.reg.GetRepository(t.Context(), "hub")
	if err != nil {
		t.Fatal(err)
	}
	placed := func(digest string) bool {
		d, _ := model.ParseDigest(digest)
		return env.reg.CheckPlacement(t.Context(), hub.Name, d) == nil
	}
	if placed(a.manifestDigest) || placed(a.layerDigest) {
		t.Error("evicted manifest and its own layer must be unplaced")
	}
	if !placed(a.configDigest) || !placed(b.manifestDigest) {
		t.Error("the config shared with image b, and b itself, must stay placed")
	}
	var blockedRows int
	if err := env.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM repository_blocked_digests WHERE digest = $1`, a.layerDigest).Scan(&blockedRows); err != nil || blockedRows != 0 {
		t.Errorf("children must not be blocked: %d %v", blockedRows, err)
	}
}

func TestAPI_BlockRefusesNonProxyAndBadDigest(t *testing.T) {
	env := newEnv(t, func(http.ResponseWriter, *http.Request) {})
	env.createJSON(t, "/api/v1/repositories", map[string]any{"name": "mine", "type": "hosted", "protocol": "oci"})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "pypi-proxy", "type": "proxy", "protocol": "pypi", "upstream_url": env.upstream.URL,
	})
	env.createOCIProxy(t, "hub", 60)
	d := "sha256:" + strings.Repeat("a", 64)
	for name, c := range map[string]struct {
		repo, digest string
		want         int
	}{
		"hosted OCI": {"mine", d, http.StatusBadRequest},
		"PyPI proxy": {"pypi-proxy", d, http.StatusBadRequest},
		"bad digest": {"hub", "sha256:zz", http.StatusBadRequest},
		"no repo":    {"nope", d, http.StatusNotFound},
	} {
		if rec := env.do(t, http.MethodPut, "/api/v1/repositories/"+c.repo+"/blocked/"+c.digest, nil, ""); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

func TestProxyOCI_VirtualTreatsBlockingMemberAsMiss(t *testing.T) {
	img := newTestImage(t, []byte("layer"), nil)
	stub := &ociStub{}
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	env.createJSON(t, "/api/v1/repositories", map[string]any{"name": "internal-oci", "type": "hosted", "protocol": "oci"})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "both", "type": "virtual", "protocol": "oci", "members": []string{"hub", "internal-oci"},
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "alone", "type": "virtual", "protocol": "oci", "members": []string{"hub"},
	})
	env.pushImage(t, "internal-oci/team/app", "1.0", img)
	if code, _ := env.block(t, "hub", img.manifestDigest, ""); code != http.StatusOK {
		t.Fatalf("block: %d", code)
	}

	before := env.requests.Load()
	path := "/manifests/" + img.manifestDigest
	rec := env.do(t, http.MethodGet, "/v2/both/team/app"+path, nil, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Docker-Content-Digest") != img.manifestDigest {
		t.Errorf("virtual with a second holder: %d, want 200 from the hosted member", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/v2/alone/team/app"+path, nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("virtual with only the blocking member: %d, want 404", rec.Code)
	}
	if n := env.requests.Load() - before; n != 0 {
		t.Errorf("%d upstream requests through a blocking member, want 0", n)
	}
}
