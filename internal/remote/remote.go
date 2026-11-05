// Package remote fetches from proxy upstreams and caches into the registry.
//
// Upstream bytes stream to the client while they are staged, and are committed
// only after the digest matches. A 404 is recorded as a negative cache entry; a
// 5xx, timeout, or truncated body leaves the cache unchanged and never replaces
// a previous hit.
package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

var (
	// ErrUpstreamNotFound means the upstream returned 404.
	ErrUpstreamNotFound = errors.New("upstream not found")

	// ErrUpstreamUnavailable means the upstream failed in a way that must not
	// poison the cache (5xx, timeout, network error, bytes that fail
	// verification).
	ErrUpstreamUnavailable = errors.New("upstream unavailable")

	// ErrUpstreamDenied means the upstream refused Sindri's credential, or
	// the lack of one, and gave no sign that the name might simply not exist.
	// It also wraps ErrUpstreamUnavailable: the member cannot be read, which
	// fails a virtual walk (a later member must not answer for a name this one
	// exists to serve), answers 502, serves a stale copy, and is never
	// negatively cached, so a fixed credential takes effect at once.
	ErrUpstreamDenied = errors.New("upstream denied access")

	// ErrUpstreamHidden means an OCI registry refused in the way Docker Hub,
	// quay.io and ghcr.io refuse names that do not exist: 401 or 403 to a
	// caller that already holds a bearer token, or at the token endpoint of an
	// anonymous proxy. It reads as not found, and is never negatively cached.
	ErrUpstreamHidden = errors.New("upstream hides the name")

	// ErrNegativeCached means a fresh negative cache entry says the name is
	// missing upstream; no upstream call was made.
	ErrNegativeCached = errors.New("negatively cached")
)

// maxMetadataBytes bounds upstream documents read into memory: PyPI project
// pages and OCI manifests. Package files and blobs stream instead. npm
// packuments have their own, larger bound.
const maxMetadataBytes = 32 << 20

// maxNPMPackumentBytes bounds an npm packument, which lists every version ever
// published and so runs to tens of megabytes for popular packages (renovate is
// 36.6 MB), past what a PyPI page or an OCI manifest ever reaches.
const maxNPMPackumentBytes = 128 << 20

// errRedirectRefused is why a pinned request stopped at a redirect.
var errRedirectRefused = errors.New("refusing a redirect to another origin")

// maxRedirects is net/http's own default, which setting CheckRedirect replaces.
const maxRedirects = 10

// Credentials looks up a secret by credentials_ref. The secret is never stored
// in remotes and never logged.
type Credentials interface {
	Lookup(ctx context.Context, ref string) (username, password string, err error)
}

// StaticCredentials is an in-memory credentials map for tests and early wiring.
type StaticCredentials map[string][2]string

// Lookup implements Credentials.
func (s StaticCredentials) Lookup(_ context.Context, ref string) (string, string, error) {
	if ref == "" {
		return "", "", nil
	}
	pair, ok := s[ref]
	if !ok {
		return "", "", fmt.Errorf("unknown credentials_ref %q", ref)
	}
	return pair[0], pair[1], nil
}

// Client talks to proxy upstreams.
type Client struct {
	reg *registry.Registry
	// http follows redirects but drops credentials leaving the first request's
	// origin; pinned refuses to leave that origin at all.
	http    *http.Client
	pinned  *http.Client
	creds   Credentials
	timeout time.Duration
	log     *slog.Logger
	// maxPackument bounds an npm packument read into memory.
	maxPackument int64
	// now is the clock the cache TTL and the refresh backoff read.
	now func() time.Time

	// backoffUntil holds, per repository, when a refresh may next be tried after
	// the upstream failed to answer one. In memory and per process, on purpose:
	// losing it costs one wait, and nothing has to agree across replicas.
	backoffMu    sync.Mutex
	backoffUntil map[string]time.Time

	// tokenCache holds bearer tokens obtained from WWW-Authenticate
	// challenges, keyed by registry host and image, since a token's scope is
	// one repository.
	tokenMu    sync.Mutex
	tokenCache map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Options wires a Client.
type Options struct {
	Registry    *registry.Registry
	HTTPClient  *http.Client
	Credentials Credentials
	// Timeout bounds a metadata request end to end, and a streamed body
	// between bytes: a large layer may take as long as it needs while it
	// keeps moving.
	Timeout time.Duration
	Logger  *slog.Logger
	// MaxNPMPackumentBytes bounds an npm packument read into memory; zero means
	// 128 MiB. Tests lower it to stay cheap.
	MaxNPMPackumentBytes int64
	// Now is the clock for cache freshness and the refresh backoff; nil means
	// time.Now. Tests inject one to cross a backoff window without waiting.
	Now func() time.Time
}

// New returns a Client.
func New(opts Options) *Client {
	if opts.Registry == nil {
		panic("remote.New: nil registry")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 0} // per-request context bounds
	}
	if opts.Credentials == nil {
		opts.Credentials = StaticCredentials{}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxNPMPackumentBytes <= 0 {
		opts.MaxNPMPackumentBytes = maxNPMPackumentBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	followed, pinned := redirectClients(opts.HTTPClient)
	return &Client{
		reg:          opts.Registry,
		http:         followed,
		pinned:       pinned,
		creds:        opts.Credentials,
		timeout:      opts.Timeout,
		log:          opts.Logger,
		maxPackument: opts.MaxNPMPackumentBytes,
		now:          opts.Now,
		backoffUntil: make(map[string]time.Time),
		tokenCache:   make(map[string]cachedToken),
	}
}

// redirectClients returns two copies of base with the redirect rules ADR 14 asks
// for. net/http alone is not enough: it keeps Authorization across a redirect to
// the same hostname on another port or scheme, and to any subdomain, and each of
// those is another origin.
func redirectClients(base *http.Client) (followed, pinned *http.Client) {
	limit := base.CheckRedirect
	if limit == nil {
		limit = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		}
	}
	f := *base
	f.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL.String(), via[0].URL.String()) {
			req.Header.Del("Authorization")
		}
		return limit(req, via)
	}
	p := *base
	p.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL.String(), via[0].URL.String()) {
			return errRedirectRefused
		}
		return limit(req, via)
	}
	return &f, &p
}

// pinnedTo returns the pinned client for one upstream. A redirect on the same
// origin is still followed, since registries move tarballs on their own host,
// but credentials stop at any hop outside the upstream's base path: on a shared
// host that path is what separates this upstream from another tenant's. Each
// hop is checked, because net/http copies the headers afresh for every one.
func (c *Client) pinnedTo(upstream string) *http.Client {
	p := *c.pinned
	check := p.CheckRedirect
	p.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := check(req, via); err != nil {
			return err
		}
		if !underUpstreamPath(upstream, req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &p
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// upstreamRequest describes one call to a proxy upstream.
type upstreamRequest struct {
	method string
	url    string
	accept string
	remote registry.Remote
	// repo names the Sindri repository, for the log line a refusal earns.
	repo string
	// tokenKey scopes a cached bearer token; empty disables bearer handling.
	tokenKey string
	// pinOrigin refuses a redirect off the origin of url (scheme and host, port
	// included), and stops credentials at a redirect outside the upstream's base
	// path. Set only for npm: tarball downloads, whose URL came from an upstream
	// document and whose own checks a redirect would otherwise undo, and
	// packument requests, whose URL is built from the configured upstream but
	// whose credentials must not follow a redirect either.
	pinOrigin bool
}

// do sends an upstream request, applying credentials only where they belong
// and completing one bearer challenge when the upstream issues it. The caller
// closes the response body.
func (c *Client) do(ctx context.Context, ur upstreamRequest) (*http.Response, error) {
	send := func(bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, ur.method, ur.url, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: building request: %w", ErrUpstreamUnavailable, err)
		}
		if ur.accept != "" {
			req.Header.Set("Accept", ur.accept)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		} else if err := c.applyCredentials(ctx, req, ur.remote); err != nil {
			return nil, err
		}
		client := c.http
		if ur.pinOrigin {
			client = c.pinnedTo(ur.remote.UpstreamURL)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(err))
		}
		return resp, nil
	}

	resp, err := send(c.cachedBearer(ur.tokenKey))
	if err != nil || resp.StatusCode != http.StatusUnauthorized || ur.tokenKey == "" {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		c.logDenied(ctx, ur.repo, http.StatusUnauthorized)
		return nil, deniedError("status 401")
	}
	token, err := c.fetchBearerToken(ctx, challenge, ur)
	if err != nil {
		return nil, err
	}
	return send(token)
}

// logDenied records a refusal of Sindri's credential: the repository and the
// status, never the URL, headers or body. A hidden name is an ordinary miss and
// is not logged.
func (c *Client) logDenied(ctx context.Context, repo string, status int) {
	c.log.WarnContext(ctx, "upstream refused the request", "repository", repo, "status", status)
}

// refusal logs err when it is a denial and returns it unchanged.
func (c *Client) refusal(ctx context.Context, repo string, status int, err error) error {
	if errors.Is(err, ErrUpstreamDenied) {
		c.logDenied(ctx, repo, status)
	}
	return err
}

// deniedError is a refusal. It wraps ErrUpstreamUnavailable as well, so every
// path that reports or recovers from an unreadable upstream treats it as one.
func deniedError(detail string) error {
	return fmt.Errorf("%w: %w: %s", ErrUpstreamDenied, ErrUpstreamUnavailable, detail)
}

// classify maps a non-200 upstream status to the error resolution acts on.
func classify(status int) error {
	switch status {
	case http.StatusNotFound:
		return ErrUpstreamNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return deniedError(fmt.Sprintf("status %d", status))
	default:
		return fmt.Errorf("%w: status %d", ErrUpstreamUnavailable, status)
	}
}

// applyCredentials attaches the remote's credentials, but only to the host
// the operator configured. Upstream documents name other hosts (a PyPI index
// may link files anywhere), and a credential must not follow those links.
func (c *Client) applyCredentials(ctx context.Context, req *http.Request, remote registry.Remote) error {
	if remote.CredentialsRef == "" || !sameOrigin(req.URL.String(), remote.UpstreamURL) {
		return nil
	}
	user, pass, err := c.creds.Lookup(ctx, remote.CredentialsRef)
	if err != nil {
		return fmt.Errorf("resolving credentials: %w", err)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	return nil
}

// sameOrigin reports whether a and b share scheme and host.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// openStream starts an upstream download whose body will stream to a client.
// The timeout covers the wait for response headers and then each gap between
// body reads, so a slow but steady large download is not cut off.
func (c *Client) openStream(ctx context.Context, ur upstreamRequest) (*http.Response, context.Context, context.CancelFunc, *time.Timer, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	idle := time.AfterFunc(c.timeout, cancel)
	resp, err := c.do(streamCtx, ur)
	if err != nil {
		idle.Stop()
		cancel()
		return nil, nil, nil, nil, err
	}
	idle.Reset(c.timeout)
	return resp, streamCtx, cancel, idle, nil
}

// newCachingBody wraps an upstream response opened by openStream.
func (c *Client) newCachingBody(ctx context.Context, resp *http.Response, cancel context.CancelFunc, idle *time.Timer,
	expected model.Digest, place func(context.Context, storage.BlobInfo) error,
) *cachingBody {
	cache, err := c.reg.NewCacheWriter(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "proxy cache unavailable; serving uncached", slog.Any("error", err))
		cache = nil
	}
	return &cachingBody{
		ctx:         ctx,
		log:         c.log,
		body:        resp.Body,
		cancel:      cancel,
		idle:        idle,
		idleTimeout: c.timeout,
		digester:    model.NewDigester(model.AlgorithmSHA256),
		expected:    expected,
		size:        resp.ContentLength,
		cache:       cache,
		place:       place,
	}
}

// CacheDecision classifies a cache entry against the remote's TTLs.
type CacheDecision int

const (
	// CacheMiss means no usable local entry; fetch upstream.
	CacheMiss CacheDecision = iota
	// CacheFreshHit means serve local without contacting upstream.
	CacheFreshHit
	// CacheFreshNegative means return not-found without contacting upstream.
	CacheFreshNegative
	// CacheStaleHit means try upstream, but fall back to the local hit on failure.
	CacheStaleHit
)

// EvaluateCache applies TTL rules to a cache entry.
func EvaluateCache(entry registry.CacheEntry, now time.Time, ttl, negativeTTL time.Duration, err error) CacheDecision {
	if err != nil || entry.Key == "" {
		return CacheMiss
	}
	age := now.Sub(entry.FetchedAt)
	switch entry.Status {
	case registry.CacheHit:
		if ttl == 0 || age <= ttl {
			return CacheFreshHit
		}
		return CacheStaleHit
	case registry.CacheMiss:
		if negativeTTL > 0 && age <= negativeTTL {
			return CacheFreshNegative
		}
		return CacheMiss
	default:
		return CacheMiss
	}
}
