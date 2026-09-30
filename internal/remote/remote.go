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

	// ErrUpstreamDenied means the upstream refused access even after any
	// credentials or bearer challenge. Docker Hub answers this way for names
	// that do not exist, so resolution treats it as not found, but it is never
	// negatively cached: a fixed credential should take effect at once.
	ErrUpstreamDenied = errors.New("upstream denied access")

	// ErrNegativeCached means a fresh negative cache entry says the name is
	// missing upstream; no upstream call was made.
	ErrNegativeCached = errors.New("negatively cached")
)

// maxMetadataBytes bounds upstream documents read into memory: PyPI project
// pages and OCI manifests. Package files and blobs stream instead.
const maxMetadataBytes = 32 << 20

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
	reg     *registry.Registry
	http    *http.Client
	creds   Credentials
	timeout time.Duration
	log     *slog.Logger

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
	return &Client{
		reg:        opts.Registry,
		http:       opts.HTTPClient,
		creds:      opts.Credentials,
		timeout:    opts.Timeout,
		log:        opts.Logger,
		tokenCache: make(map[string]cachedToken),
	}
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
	// tokenKey scopes a cached bearer token; empty disables bearer handling.
	tokenKey string
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
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
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
		return nil, fmt.Errorf("%w: status 401", ErrUpstreamDenied)
	}
	token, err := c.fetchBearerToken(ctx, challenge, ur)
	if err != nil {
		return nil, err
	}
	return send(token)
}

// classify maps a non-200 upstream status to the error resolution acts on.
func classify(status int) error {
	switch status {
	case http.StatusNotFound:
		return ErrUpstreamNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrUpstreamDenied, status)
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

// cacheKeyPyPIIndex is the remote_cache key for a project's simple index.
func cacheKeyPyPIIndex(project string) string {
	return "pypi:index:" + project
}

// cacheKeyOCIManifest is the remote_cache key for an OCI tag or digest ref.
func cacheKeyOCIManifest(image, reference string) string {
	return "oci:manifest:" + image + ":" + reference
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
