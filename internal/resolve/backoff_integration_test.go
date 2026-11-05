//go:build integration

package resolve_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/remote"
)

// testClock is a clock a test moves by hand, so a backoff window is crossed
// without waiting for it. It starts at the real time, since the cache TTL is
// compared with timestamps the database wrote.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func withClock(c *testClock) envOption {
	return func(o *remote.Options) { o.Now = c.Now }
}

// newStaleEnv is a gone environment on a hand-moved clock, with a TTL set and the
// cached document aged past it.
func newStaleEnv(t *testing.T, p goneProtocol, extra ...envOption) (*goneEnv, *testClock) {
	t.Helper()
	clock := newTestClock()
	e := newGoneEnv(t, p, append([]envOption{withClock(clock)}, extra...)...)
	e.setTTL(t)
	e.age(t)
	return e, clock
}

// breakCachedDocument points the cached document of the package at a blob whose
// bytes do not exist, so loading it fails. A read that still succeeds did not
// load it.
func (e *goneEnv) breakCachedDocument(t *testing.T) {
	t.Helper()
	missing := "sha256:" + strings.Repeat("0", 64)
	ctx := t.Context()
	if _, err := e.db.Pool().Exec(ctx, `INSERT INTO blobs (digest, size, storage_backend) VALUES ($1, 1, 'memory') ON CONFLICT DO NOTHING`, missing); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool().Exec(ctx, `UPDATE remote_cache SET blob_digest = $1 WHERE status = 'hit'`, missing); err != nil {
		t.Fatal(err)
	}
	// A file the document would have to be consulted for: 404 means it was parsed.
	if got := e.status(t, "up", e.p.filePath("9.9.9")); got == http.StatusNotFound || got == http.StatusOK {
		t.Fatalf("the document still loads after breaking it (%d)", got)
	}
}

func TestProxy_OutageWaitsOnlyOncePerWindow(t *testing.T) {
	for _, p := range goneProtocols() {
		t.Run(p.name, func(t *testing.T) {
			e, clock := newStaleEnv(t, p)
			e.up.list("1.0.0", "2.0.0", "3.0.0") // 3.0.0 is listed, never fetched
			e.age(t)
			e.up.dropConnections()

			before := e.requests.Load()
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusOK {
				t.Fatalf("first read past the TTL = %d, want the placed copy (200)", got)
			}
			// net/http retries a GET once when a reused connection is cut, so the
			// one failed refresh can show as two requests.
			if got := e.requests.Load() - before; got < 1 || got > 2 {
				t.Fatalf("first read made %d upstream requests, want the one failed refresh", got)
			}

			before = e.requests.Load()
			for _, repo := range []string{"up", "all"} {
				for _, v := range []string{"1.0.0", "2.0.0"} {
					if got := e.status(t, repo, p.filePath(v)); got != http.StatusOK {
						t.Errorf("GET %s %s during the backoff = %d, want the placed copy (200)", repo, v, got)
					}
				}
				// Nothing placed to serve: unavailable at once, as in a real outage.
				if got := e.status(t, repo, p.filePath("3.0.0")); got != http.StatusBadGateway {
					t.Errorf("GET %s of an unplaced file during the backoff = %d, want 502", repo, got)
				}
			}
			if got := e.requests.Load() - before; got != 0 {
				t.Fatalf("%d upstream requests during the backoff window, want none", got)
			}

			clock.advance(31 * time.Second)
			before = e.requests.Load()
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusOK {
				t.Fatalf("read after the window = %d, want the placed copy (200)", got)
			}
			if got := e.requests.Load() - before; got < 1 || got > 2 {
				t.Fatalf("read after the window made %d upstream requests, want one retry", got)
			}
		})
	}
}

func TestProxy_FreshPlacedReadDoesNotLoadTheDocument(t *testing.T) {
	cases := []struct {
		name string
		// prepare leaves the cache fresh, and returns the versions that must
		// answer 200 and 404.
		prepare      func(e *goneEnv, t *testing.T)
		served, gone []string
	}{
		{"placed file", func(e *goneEnv, t *testing.T) {}, []string{"1.0.0", "2.0.0"}, nil},
		{"flagged file", func(e *goneEnv, t *testing.T) {
			// A refresh that no longer lists 1.0.0 flags it and leaves the cache fresh.
			e.age(t)
			e.up.list("2.0.0")
			if got := e.status(t, "up", e.p.filePath("2.0.0")); got != http.StatusOK {
				t.Fatalf("refreshing read = %d, want 200", got)
			}
		}, []string{"2.0.0"}, []string{"1.0.0"}},
	}
	for _, p := range goneProtocols() {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				e, _ := newStaleEnv(t, p)
				if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = now()`); err != nil {
					t.Fatal(err)
				}
				tc.prepare(e, t)
				e.breakCachedDocument(t)
				before := e.requests.Load()
				for _, repo := range []string{"up", "all"} {
					for _, v := range tc.served {
						if got := e.status(t, repo, p.filePath(v)); got != http.StatusOK {
							t.Errorf("GET %s %s = %d, want 200 without the document", repo, v, got)
						}
					}
					for _, v := range tc.gone {
						if got := e.status(t, repo, p.filePath(v)); got != http.StatusNotFound {
							t.Errorf("GET %s %s = %d, want 404 without the document", repo, v, got)
						}
					}
				}
				if got := e.requests.Load() - before; got != 0 {
					t.Errorf("%d upstream requests on a fresh read, want none", got)
				}
			})
		}
	}
}

func TestProxy_FileGoneUpstreamIsNotFoundAfterRecovery(t *testing.T) {
	for _, p := range goneProtocols() {
		t.Run(p.name, func(t *testing.T) {
			e, clock := newStaleEnv(t, p)
			e.up.dropConnections()
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusOK {
				t.Fatalf("read during the outage = %d, want the placed copy (200)", got)
			}

			// Upstream recovers and has removed 1.0.0. Until the window ends the
			// refresh is not tried, so the copy keeps serving: the documented cost.
			e.up.list("2.0.0")
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusOK {
				t.Fatalf("read inside the window = %d, want the placed copy (200)", got)
			}

			clock.advance(31 * time.Second)
			for _, repo := range []string{"up", "all"} {
				if got := e.status(t, repo, p.filePath("1.0.0")); got != http.StatusNotFound {
					t.Errorf("GET %s of the removed file after a successful refresh = %d, want 404", repo, got)
				}
				if got := e.status(t, repo, p.filePath("2.0.0")); got != http.StatusOK {
					t.Errorf("GET %s of the kept file = %d, want 200", repo, got)
				}
			}
		})
	}
}

func TestProxy_AnswerThatIsNotAnOutageDoesNotBackOffTheRepository(t *testing.T) {
	oversize := `{"name":"demo","pad":"` + strings.Repeat("x", 8192) + `"}`
	cases := []struct {
		name string
		do   func(u *goneUpstream)
		only string
		opts []envOption
	}{
		{"unparsable", func(u *goneUpstream) { u.garble() }, "", nil},
		{"another project's document", func(u *goneUpstream) { u.serveDoc(`{"name":"other","files":[],"versions":{}}`) }, "", nil},
		{"unauthenticated", func(u *goneUpstream) { u.answer(http.StatusUnauthorized) }, "", nil},
		{"refused credential", func(u *goneUpstream) { u.answer(http.StatusForbidden) }, "", nil},
		{"server error", func(u *goneUpstream) { u.answer(http.StatusServiceUnavailable) }, "", nil},
		{"rate limited", func(u *goneUpstream) { u.answer(http.StatusTooManyRequests) }, "", nil},
		{"oversize", func(u *goneUpstream) { u.serveDoc(oversize) }, "npm",
			[]envOption{func(o *remote.Options) { o.MaxNPMPackumentBytes = 4096 }}},
	}
	for _, p := range goneProtocols() {
		for _, tc := range cases {
			if tc.only != "" && tc.only != p.name {
				continue
			}
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				e, _ := newStaleEnv(t, p, tc.opts...)
				tc.do(e.up)
				// Each read refreshes: a bad document or a 5xx is not an outage, so no read
				// is turned away unasked, and the placed copy keeps serving.
				for i := range 3 {
					before := e.requests.Load()
					if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusOK {
						t.Fatalf("read %d = %d, want the placed copy (200)", i, got)
					}
					if got := e.requests.Load() - before; got != 1 {
						t.Fatalf("read %d made %d upstream requests, want 1: the repository is in backoff", i, got)
					}
				}
			})
		}
	}
}
