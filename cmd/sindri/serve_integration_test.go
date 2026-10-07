//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const bootstrapToken = "m7-bootstrap-token"

// stack is a running `serve` built by the real wiring, driven through its
// test seams instead of process signals.
type stack struct {
	t      *testing.T
	base   string
	reload chan os.Signal
	done   chan error
	path   string
}

// startServe writes cfgBody (with the database and listener filled in) to a
// file and runs serve on it until the test ends.
func startServe(t *testing.T, dbURL, cfgBody string, extra func(*serveHooks)) *stack {
	t.Helper()
	return startServeAuth(t, dbURL, "", cfgBody, extra)
}

// startServeAuth is startServe with extra lines for the auth section, each
// indented two spaces.
func startServeAuth(t *testing.T, dbURL, authLines, cfgBody string, extra func(*serveHooks)) *stack {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sindri.yaml")
	header := fmt.Sprintf("listen: 127.0.0.1:0\nstorage: %s\ndatabase: %s\nlog: warn\nauth:\n  static_token: %s\n%s",
		filepath.Join(dir, "data"), dbURL, bootstrapToken, authLines)
	if err := os.WriteFile(path, []byte(header+cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &stack{t: t, reload: make(chan os.Signal, 1), done: make(chan error, 1), path: path}
	addr := make(chan string, 1)
	hooks := serveHooks{
		reload:    s.reload,
		listening: func(a string) { addr <- a },
		build:     buildOptions{Blobs: memstore.New()},
	}
	if extra != nil {
		extra(&hooks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { s.done <- serve(ctx, path, hooks) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-s.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("serve did not stop")
		}
	})

	select {
	case a := <-addr:
		s.base = "http://" + a
	case err := <-s.done:
		t.Fatalf("serve exited before listening: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not start listening")
	}
	return s
}

// rewrite replaces the desired-state part of the file and signals a reload.
func (s *stack) rewrite(cfgBody string) {
	s.t.Helper()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(raw), "\n# --- desired\n")
	if err := os.WriteFile(s.path, []byte(head+"\n# --- desired\n"+cfgBody), 0o600); err != nil {
		s.t.Fatal(err)
	}
	s.reload <- os.Interrupt // any value: the channel carries no meaning but "now"
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (s *stack) request(method, path string, body io.Reader, setAuth func(*http.Request)) response {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.base+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	if setAuth != nil {
		setAuth(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// repoNames lists repositories through the management API.
func (s *stack) repoNames() []string {
	s.t.Helper()
	resp := s.request(http.MethodGet, "/api/v1/repositories?limit=200", nil, bearer(bootstrapToken))
	if resp.status != http.StatusOK {
		s.t.Fatalf("list repositories: %d %s", resp.status, resp.body)
	}
	var page struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.body, &page); err != nil {
		s.t.Fatal(err)
	}
	names := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		names = append(names, it.Name)
	}
	return names
}

// eventually polls cond until it holds; a reload runs on its own goroutine.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Gate test 3: `serve --config` creates the declared repositories before it
// listens, with no apply step, and a reload signal re-reconciles. A reload
// that fails to load keeps the running state.
func TestServe_ReconcilesOnBootAndOnSIGHUP(t *testing.T) {
	dbURL := testsupport.StartPostgres(t)
	s := startServe(t, dbURL, `
# --- desired
repos:
  internal-py: { protocol: pypi }
  docker:      { protocol: oci }
`, nil)

	if got := s.repoNames(); !slices.Equal(got, []string{"docker", "internal-py"}) {
		t.Fatalf("after boot: %v, want the two declared repositories", got)
	}

	s.rewrite(`
repos:
  internal-py: { protocol: pypi }
  docker:      { protocol: oci }
  extra:       { protocol: oci }
lifecycle:
  stages: [dev, prod]
`)
	eventually(t, "the reload to create extra", func() bool {
		return slices.Contains(s.repoNames(), "extra")
	})
	stages := s.request(http.MethodGet, "/api/v1/lifecycle/stages", nil, bearer(bootstrapToken))
	if !strings.Contains(string(stages.body), `"prod"`) {
		t.Fatalf("stages after reload: %s", stages.body)
	}

	// A broken file is logged and ignored; the server keeps serving the
	// last good state.
	s.rewrite("repos:\n  bad: { members: [x], upstream: pypi }\n")
	time.Sleep(500 * time.Millisecond)
	if got := s.repoNames(); !slices.Equal(got, []string{"docker", "extra", "internal-py"}) {
		t.Fatalf("after a bad reload: %v", got)
	}
	if r := s.request(http.MethodGet, "/healthz", nil, nil); r.status != http.StatusOK {
		t.Fatalf("server unhealthy after a bad reload: %d", r.status)
	}
}
