package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
)

func TestMapRegistryError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantLeak   string
	}{
		{name: "version exists is a conflict", err: registry.ErrVersionExists, wantStatus: http.StatusConflict},
		{name: "file exists is a conflict", err: registry.ErrFileExists, wantStatus: http.StatusConflict},
		{name: "not found", err: registry.ErrNotFound, wantStatus: http.StatusNotFound},
		{name: "wrong protocol hides the repository", err: registry.ErrWrongProtocol, wantStatus: http.StatusNotFound},
		{name: "not placed hides the blob", err: registry.ErrNotPlaced, wantStatus: http.StatusNotFound},
		{name: "read only", err: registry.ErrReadOnly, wantStatus: http.StatusMethodNotAllowed},
		{name: "invalid input", err: registry.ErrInvalidInput, wantStatus: http.StatusBadRequest},
		{name: "digest mismatch", err: cas.ErrDigestMismatch, wantStatus: http.StatusBadRequest},
		{name: "refused tarball", err: fmt.Errorf("%w: bad header", errTarballRefused), wantStatus: http.StatusBadRequest},
		{name: "oversized unpack is the client's", err: errUnpackedTooLarge, wantStatus: http.StatusBadRequest},
		{name: "wrapped sentinel", err: fmt.Errorf("publishing: %w", registry.ErrNotFound), wantStatus: http.StatusNotFound},
		{name: "unknown error leaks nothing", err: errors.New("pq: password for db-host-7 failed"), wantStatus: http.StatusInternalServerError, wantLeak: "db-host-7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			mapRegistryError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Fatalf("body = %q, want a JSON error field (err %v)", rec.Body.String(), err)
			}
			if tc.wantLeak != "" && strings.Contains(rec.Body.String(), tc.wantLeak) {
				t.Fatalf("body %q leaks %q", rec.Body.String(), tc.wantLeak)
			}
		})
	}
}

type rejectAll struct{}

func (rejectAll) Authenticate(context.Context, *http.Request) (auth.Principal, error) {
	return auth.Principal{}, errors.New("no credential")
}

type noVerifier struct{}

func (noVerifier) Verify(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, errors.New("unused")
}

func TestHandler_NoCredentialIs401(t *testing.T) {
	t.Parallel()
	h := Handler(Options{Resolver: &resolve.Resolver{}, Auth: rejectAll{}, Verifier: noVerifier{}})
	cases := []struct {
		name string
		path string
	}{
		{name: "packument", path: "/npm/r/foo"},
		{name: "tarball", path: "/npm/r/foo/-/foo-1.0.0.tgz"},
		{name: "whoami", path: "/npm/r/-/whoami"},
		{name: "unmatched path reveals no routes", path: "/npm/r/-/nonsense/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="sindri"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
		})
	}
}

func TestHandler_LoginRoutesBypassAuthentication(t *testing.T) {
	t.Parallel()
	h := Handler(Options{Resolver: &resolve.Resolver{}, Auth: rejectAll{}, Verifier: noVerifier{}})
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "web login", method: http.MethodPost, path: "/npm/r/-/v1/login", want: http.StatusNotFound},
		// An empty body reaches the handler's own check; the middleware would
		// have answered 401 with a challenge.
		{name: "user login", method: http.MethodPut, path: "/npm/r/-/user/org.couchdb.user:alice", want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (reached the handler, not the middleware)", rec.Code, tc.want)
			}
		})
	}
}
