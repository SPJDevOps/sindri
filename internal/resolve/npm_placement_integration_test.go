//go:build integration

package resolve_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/spj/sindri/internal/registry"
)

// I2 for npm: deduplication makes the bytes of a tarball placed in repository A
// physically present for everyone, so a read through any other repository has to
// be refused by placement, not by the bytes being absent. Each case gives the
// other repository every reason to serve A's bytes (the same name, the same
// version, an upstream that advertises their exact integrity) and expects it not
// to.
func TestNPM_BytesPlacedInOneRepositoryAreNotReadThroughAnother(t *testing.T) {
	const tarballPath = "/@acme/ui/-/ui-1.0.0.tgz"

	tests := []struct {
		name string
		// arrange builds the other repository and returns the path to read through it.
		arrange func(t *testing.T, e *testEnv, tgz []byte) string
		want    []int // acceptable statuses; none is 200
	}{
		{
			name: "AHostedRepositoryThatHoldsNoSuchPackage",
			arrange: func(t *testing.T, e *testEnv, _ []byte) string {
				e.createHostedNPM(t, "b")
				return "/npm/b" + tarballPath
			},
			want: []int{http.StatusNotFound},
		},
		{
			name: "AHostedRepositoryThatHoldsTheSamePackageAtAnotherVersion",
			arrange: func(t *testing.T, e *testEnv, _ []byte) string {
				e.createHostedNPM(t, "b")
				e.publishNPM(t, "b", "@acme/ui", "2.0.0")
				return "/npm/b" + tarballPath
			},
			want: []int{http.StatusNotFound},
		},
		{
			name: "AProxyWhoseUpstreamAdvertisesTheSameIntegrityButCannotServeIt",
			arrange: func(t *testing.T, e *testEnv, tgz []byte) string {
				up := e.createProxyNPM(t, "b", 3600)
				up.setDocument("/@acme%2fui", npmDoc(t, "@acme/ui", map[string]map[string]any{
					"1.0.0": {"tarball": up.url + "/gone/ui-1.0.0.tgz", "integrity": sri512(tgz), "shasum": shasum(tgz)},
				}, map[string]string{"latest": "1.0.0"}))
				return "/npm/b" + tarballPath
			},
			want: []int{http.StatusNotFound, http.StatusBadGateway},
		},
		{
			name: "AProxyWhoseUpstreamIsDown",
			arrange: func(t *testing.T, e *testEnv, tgz []byte) string {
				up := e.createProxyNPM(t, "b", 3600)
				up.setDocument("/@acme%2fui", npmDoc(t, "@acme/ui", map[string]map[string]any{
					"1.0.0": {"tarball": up.url + "/gone/ui-1.0.0.tgz", "integrity": sri512(tgz)},
				}, map[string]string{"latest": "1.0.0"}))
				up.configure(func(s *npmStub) { s.tarballStatus = http.StatusServiceUnavailable })
				return "/npm/b" + tarballPath
			},
			want: []int{http.StatusBadGateway},
		},
		{
			name: "AVirtualRepositoryWhoseMembersDoNotIncludeTheRepository",
			arrange: func(t *testing.T, e *testEnv, _ []byte) string {
				e.createHostedNPM(t, "b")
				e.createProxyNPM(t, "npmjs", 3600)
				e.createVirtualNPM(t, "all", "b", "npmjs")
				return "/npm/all" + tarballPath
			},
			want: []int{http.StatusNotFound},
		},
		{
			name: "AVirtualRepositoryOverAProxyThatAdvertisesTheSameIntegrity",
			arrange: func(t *testing.T, e *testEnv, tgz []byte) string {
				up := e.createProxyNPM(t, "npmjs", 3600)
				up.setDocument("/@acme%2fui", npmDoc(t, "@acme/ui", map[string]map[string]any{
					"1.0.0": {"tarball": up.url + "/gone/ui-1.0.0.tgz", "integrity": sri512(tgz)},
				}, map[string]string{"latest": "1.0.0"}))
				e.createVirtualNPM(t, "all", "npmjs")
				return "/npm/all" + tarballPath
			},
			want: []int{http.StatusNotFound, http.StatusBadGateway},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, http.NotFound)
			e.createHostedNPM(t, "a")
			tgz := e.publishNPM(t, "a", "@acme/ui", "1.0.0")

			path := tc.arrange(t, e, tgz)
			rec := e.do(t, http.MethodGet, path, nil, "")
			if rec.Code == http.StatusOK || !containsStatus(tc.want, rec.Code) {
				t.Fatalf("GET %s = %d %s, want one of %v", path, rec.Code, rec.Body.String(), tc.want)
			}
			if bytes.Contains(rec.Body.Bytes(), tgz) {
				t.Fatalf("the response to GET %s carries repository a's tarball", path)
			}
			if _, _, err := e.reg.GetNPMTarball(t.Context(), "b", "@acme/ui", "ui-1.0.0.tgz"); err != nil && !errors.Is(err, registry.ErrNotFound) {
				t.Fatalf("GetNPMTarball on b: %v", err)
			} else if err == nil {
				t.Fatal("repository b holds a placement for a's tarball")
			}
		})
	}
}

func containsStatus(want []int, got int) bool {
	for _, w := range want {
		if w == got {
			return true
		}
	}
	return false
}
