//go:build integration

package resolve_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestSubjects_ProxyCacheCreatesNoSubject: an image pulled through a proxy is
// cached into the CAS but is not ours (invariant I4), so it gets no subject,
// and evidence cannot be attached to it.
func TestSubjects_ProxyCacheCreatesNoSubject(t *testing.T) {
	stub := &ociStub{}
	img := newTestImage(t, []byte("upstream layer"), nil)
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)

	pull := env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/latest", nil, "")
	if pull.Code != http.StatusOK {
		t.Fatalf("proxied pull: %d %s", pull.Code, pull.Body.String())
	}

	subject := env.do(t, http.MethodGet, "/api/v1/subjects/"+img.manifestDigest, nil, "")
	if subject.Code != http.StatusNotFound {
		t.Fatalf("subject of proxied manifest: %d %s, want 404", subject.Code, subject.Body.String())
	}
	attach := env.do(t, http.MethodPost, "/api/v1/subjects/"+img.manifestDigest+"/evidence?type=sbom",
		[]byte(`{"sbom":true}`), "application/json")
	if attach.Code != http.StatusNotFound {
		t.Fatalf("evidence on proxied manifest: %d %s, want 404", attach.Code, attach.Body.String())
	}
}

// TestLifecycle_ProxyRepositoryCannotPromote is invariant I4 on the promotion
// path: even when the same digest is ours in a hosted repository, naming the
// proxy repository that also caches it is refused.
func TestLifecycle_ProxyRepositoryCannotPromote(t *testing.T) {
	stub := &ociStub{}
	img := newTestImage(t, []byte("shared layer"), nil)
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	env.createJSON(t, "/api/v1/repositories", map[string]any{"name": "internal", "protocol": "oci"})

	env.pushImage(t, "internal", "1.0", img)
	if pull := env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/latest", nil, ""); pull.Code != http.StatusOK {
		t.Fatalf("proxied pull: %d", pull.Code)
	}

	stages := env.do(t, http.MethodPut, "/api/v1/lifecycle/stages",
		[]byte(`{"stages":[{"name":"prod","gates":[]}]}`), "application/json")
	if stages.Code != http.StatusOK {
		t.Fatalf("set stages: %d %s", stages.Code, stages.Body.String())
	}

	viaProxy := env.do(t, http.MethodPost, "/api/v1/subjects/"+img.manifestDigest+"/promote",
		[]byte(`{"repository":"hub","stage":"prod"}`), "application/json")
	if viaProxy.Code != http.StatusConflict || !strings.Contains(viaProxy.Body.String(), "NOT_PROMOTABLE") {
		t.Fatalf("promote via proxy: %d %s, want 409 NOT_PROMOTABLE", viaProxy.Code, viaProxy.Body.String())
	}
	viaHosted := env.do(t, http.MethodPost, "/api/v1/subjects/"+img.manifestDigest+"/promote",
		[]byte(`{"repository":"internal","stage":"prod"}`), "application/json")
	if viaHosted.Code != http.StatusCreated {
		t.Fatalf("promote via hosted: %d %s, want 201", viaHosted.Code, viaHosted.Body.String())
	}
}
