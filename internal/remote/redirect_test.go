package remote

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spj/sindri/internal/registry"
)

// redirectFixture is an upstream that redirects to a second server on the same
// host and another port. Go's own client keeps Authorization across that hop,
// because it compares hostnames and ignores the port, so these tests also
// guard what a plain http.Client would get wrong.
type redirectFixture struct {
	client   *Client
	remote   registry.Remote
	upstream *httptest.Server
	offsite  *httptest.Server
	hits     atomic.Int32
	sawAuth  atomic.Bool
}

func newRedirectFixture(t *testing.T) *redirectFixture {
	t.Helper()
	f := &redirectFixture{}
	f.offsite = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			f.sawAuth.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.offsite.Close)
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, f.offsite.URL+"/file", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/file", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/file":
			if _, _, ok := r.BasicAuth(); !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.upstream.Close)
	f.client = New(Options{
		Registry:    &registry.Registry{},
		Credentials: StaticCredentials{"mirror": {"user", "s3cret"}},
	})
	f.remote = registry.Remote{UpstreamURL: f.upstream.URL, CredentialsRef: "mirror"}
	return f
}

// get returns the final status of a request through the client.
func (f *redirectFixture) get(t *testing.T, path string, pin bool) (int, error) {
	t.Helper()
	resp, err := f.client.do(t.Context(), upstreamRequest{
		method:    http.MethodGet,
		url:       f.upstream.URL + path,
		remote:    f.remote,
		pinOrigin: pin,
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func TestDo_RedirectOffOriginCarriesNoCredentials(t *testing.T) {
	t.Parallel()
	f := newRedirectFixture(t)
	status, err := f.get(t, "/away", false)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != http.StatusOK || f.hits.Load() != 1 {
		t.Fatalf("status %d, %d offsite hits; want the redirect followed", status, f.hits.Load())
	}
	if f.sawAuth.Load() {
		t.Fatal("the upstream's credentials followed a redirect off its origin")
	}
}

func TestDo_PinnedRequestRefusesARedirectOffOrigin(t *testing.T) {
	t.Parallel()
	f := newRedirectFixture(t)
	_, err := f.get(t, "/away", true)
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("do = %v, want ErrUpstreamUnavailable", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("a pinned request reached another origin")
	}
}

func TestDo_RedirectOnTheSameOriginKeepsCredentials(t *testing.T) {
	t.Parallel()
	for _, pin := range []bool{false, true} {
		f := newRedirectFixture(t)
		status, err := f.get(t, "/here", pin)
		if err != nil {
			t.Fatalf("pin=%v: do: %v", pin, err)
		}
		if status != http.StatusOK {
			t.Fatalf("pin=%v: status %d, want credentials kept on the same origin", pin, status)
		}
	}
}

func TestDo_RedirectLoopStillFails(t *testing.T) {
	t.Parallel()
	for _, pin := range []bool{false, true} {
		f := newRedirectFixture(t)
		if _, err := f.get(t, "/loop", pin); !errors.Is(err, ErrUpstreamUnavailable) {
			t.Fatalf("pin=%v: do = %v, want ErrUpstreamUnavailable", pin, err)
		}
	}
}

// A redirect policy a caller injected still runs.
func TestNew_KeepsAnInjectedRedirectPolicy(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused by the caller")
	c := New(Options{
		Registry: &registry.Registry{},
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return refused
		}},
	})
	f := newRedirectFixture(t)
	resp, err := c.do(t.Context(), upstreamRequest{method: http.MethodGet, url: f.upstream.URL + "/here", remote: registry.Remote{UpstreamURL: f.upstream.URL}})
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, refused) {
		t.Fatalf("do = %v, want the injected policy's error", err)
	}
}
