package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

// dockerHubHosts are the registry hosts that keep official images under
// library/. A Docker client adds that prefix only when it talks to Docker Hub
// directly, never to a registry like Sindri, so the proxy adds it.
var dockerHubHosts = map[string]bool{
	"registry-1.docker.io":    true,
	"docker.io":               true,
	"index.docker.io":         true,
	"registry.hub.docker.com": true,
}

// upstreamImagePath maps the image a client asked a proxy repository for onto
// the upstream's name for it. An empty image means the proxy repository's own
// name is the image.
func upstreamImagePath(remote registry.Remote, repoName, image string) string {
	path := strings.Trim(image, "/")
	if path == "" {
		path = repoName
	}
	if !strings.Contains(path, "/") {
		if u, err := url.Parse(remote.UpstreamURL); err == nil && dockerHubHosts[strings.ToLower(u.Hostname())] {
			return "library/" + path
		}
	}
	return path
}

// ociUpstream builds the request for an OCI path under an upstream image.
func ociUpstream(remote registry.Remote, repoName, image, method, suffix, accept string) upstreamRequest {
	path := upstreamImagePath(remote, repoName, image)
	u := strings.TrimRight(remote.UpstreamURL, "/") + "/v2/" + path + "/" + suffix
	return upstreamRequest{
		method:   method,
		url:      u,
		accept:   accept,
		remote:   remote,
		tokenKey: upstreamHost(remote.UpstreamURL) + "|" + path,
	}
}

// FetchOCIManifest returns a manifest from a proxy repository, fetching from
// upstream on miss and caching into the CAS.
//
// A digest reference is immutable, so a local placement answers it outright.
// A tag is mutable, so it goes through remote_cache: fresh serves the local
// tag, stale asks upstream again and falls back to the local tag only when
// upstream is unavailable.
func (c *Client) FetchOCIManifest(ctx context.Context, repoName, image, reference, accept string) (registry.Manifest, error) {
	if _, err := model.ParseDigest(reference); err == nil {
		if man, err := c.reg.GetManifest(ctx, repoName, image, reference); err == nil {
			return man, nil
		} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
			return registry.Manifest{}, err
		}
	}

	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return registry.Manifest{}, err
	}

	key := cacheKeyOCIManifest(image, reference)
	entry, entryErr := c.reg.GetCacheEntry(ctx, repoName, key)
	switch EvaluateCache(entry, time.Now().UTC(), remote.TTL, remote.NegativeTTL, entryErr) {
	case CacheFreshHit:
		if man, err := c.reg.GetManifest(ctx, repoName, image, reference); err == nil {
			return man, nil
		}
		// A hit row without placement: refetch.
	case CacheFreshNegative:
		return registry.Manifest{}, ErrNegativeCached
	case CacheStaleHit, CacheMiss:
	}

	man, err := c.fetchAndCacheOCIManifest(ctx, repoName, image, reference, accept, remote)
	if err == nil {
		return man, nil
	}
	if errors.Is(err, ErrUpstreamNotFound) || errors.Is(err, ErrUpstreamDenied) {
		return registry.Manifest{}, err
	}
	// Upstream is down: a tag cached earlier still serves.
	if local, lerr := c.reg.GetManifest(ctx, repoName, image, reference); lerr == nil {
		return local, nil
	}
	return registry.Manifest{}, err
}

func (c *Client) fetchAndCacheOCIManifest(ctx context.Context, repoName, image, reference, accept string, remote registry.Remote) (registry.Manifest, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	resp, err := c.do(ctx, ociUpstream(remote, repoName, image, http.MethodGet, "manifests/"+reference, accept))
	if err != nil {
		return registry.Manifest{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	key := cacheKeyOCIManifest(image, reference)
	if resp.StatusCode != http.StatusOK {
		err := classify(resp.StatusCode)
		if errors.Is(err, ErrUpstreamNotFound) {
			if perr := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheMiss, ""); perr != nil {
				return registry.Manifest{}, perr
			}
		}
		return registry.Manifest{}, err
	}

	body, err := readMetadata(resp.Body)
	if err != nil {
		return registry.Manifest{}, err
	}
	man, err := c.reg.PlaceCachedManifest(ctx, repoName, image, reference, resp.Header.Get("Content-Type"), body)
	if err != nil {
		if errors.Is(err, registry.ErrInvalidInput) {
			// Upstream sent a manifest that does not match its reference or
			// does not parse: a bad upstream, not a bad client request.
			return registry.Manifest{}, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
		}
		return registry.Manifest{}, err
	}
	if err := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheHit, man.Digest.String()); err != nil {
		return registry.Manifest{}, err
	}
	return man, nil
}

// readMetadata reads a bounded upstream document, failing rather than
// truncating when it is larger than the bound.
func readMetadata(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading body: %w", ErrUpstreamUnavailable, err)
	}
	if len(body) > maxMetadataBytes {
		return nil, fmt.Errorf("%w: document exceeds %d bytes", ErrUpstreamUnavailable, maxMetadataBytes)
	}
	return body, nil
}

// StatOCIBlob reports a blob's size for a proxy repository: from the local
// placement when cached, otherwise from an upstream HEAD. A HEAD never
// downloads or caches bytes.
func (c *Client) StatOCIBlob(ctx context.Context, repoName, image string, digest model.Digest) (int64, error) {
	if info, err := c.reg.StatBlob(ctx, repoName, digest); err == nil {
		return info.Size, nil
	} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		return 0, err
	}

	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return 0, err
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.do(ctx, ociUpstream(remote, repoName, image, http.MethodHead, "blobs/"+digest.String(), ""))
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, classify(resp.StatusCode)
	}
	if resp.ContentLength >= 0 {
		return resp.ContentLength, nil
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("%w: HEAD without a usable Content-Length", ErrUpstreamUnavailable)
	}
	return size, nil
}

// FetchOCIBlob returns a blob from a proxy repository. On a miss the upstream
// body streams straight back while it is staged, and is placed in the proxy
// repository once its digest verifies. The size is -1 when upstream did not
// declare one.
func (c *Client) FetchOCIBlob(ctx context.Context, repoName, image string, digest model.Digest) (io.ReadCloser, int64, error) {
	if info, rc, err := c.reg.GetBlob(ctx, repoName, digest); err == nil {
		return rc, info.Size, nil
	} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		return nil, 0, err
	}

	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return nil, 0, err
	}

	resp, streamCtx, cancel, idle, err := c.openStream(ctx, ociUpstream(remote, repoName, image, http.MethodGet, "blobs/"+digest.String(), ""))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		idle.Stop()
		cancel()
		return nil, 0, classify(resp.StatusCode)
	}
	body := c.newCachingBody(streamCtx, resp, cancel, idle, digest, func(ctx context.Context, info storage.BlobInfo) error {
		return c.reg.LinkCachedBlob(ctx, repoName, info)
	})
	return body, resp.ContentLength, nil
}

// cachedBearer returns a live token for key, or "".
func (c *Client) cachedBearer(key string) string {
	if key == "" {
		return ""
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if tok, ok := c.tokenCache[key]; ok && time.Now().Before(tok.expiresAt) {
		return tok.token
	}
	return ""
}

func upstreamHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return strings.ToLower(u.Host)
}

// fetchBearerToken completes a WWW-Authenticate bearer challenge. The realm
// comes from the upstream, so it is trusted as far as the upstream is, with
// one limit: credentials go to it only over https or on the upstream's own
// origin, never in clear text to a third host.
func (c *Client) fetchBearerToken(ctx context.Context, challenge string, ur upstreamRequest) (string, error) {
	realm, service, scope := parseAuthChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("%w: bearer challenge without a realm", ErrUpstreamUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm, nil)
	if err != nil {
		return "", fmt.Errorf("%w: building token request: %w", ErrUpstreamUnavailable, err)
	}
	q := req.URL.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	req.URL.RawQuery = q.Encode()
	if ur.remote.CredentialsRef != "" && (req.URL.Scheme == "https" || sameOrigin(realm, ur.remote.UpstreamURL)) {
		user, pass, err := c.creds.Lookup(ctx, ur.remote.CredentialsRef)
		if err != nil {
			return "", fmt.Errorf("resolving credentials: %w", err)
		}
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: token endpoint: %w", ErrUpstreamUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("%w: token endpoint status %d", ErrUpstreamDenied, resp.StatusCode)
		}
		return "", fmt.Errorf("%w: token endpoint status %d", ErrUpstreamUnavailable, resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("%w: decoding token: %w", ErrUpstreamUnavailable, err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("%w: empty token response", ErrUpstreamUnavailable)
	}
	// Docker's token spec defaults to 60 seconds when expires_in is absent.
	lifetime := 60 * time.Second
	if payload.ExpiresIn > 0 {
		lifetime = time.Duration(payload.ExpiresIn) * time.Second
	}
	// Renew a little early so a token does not expire mid-request.
	if lifetime > 20*time.Second {
		lifetime -= 10 * time.Second
	}
	c.tokenMu.Lock()
	c.tokenCache[ur.tokenKey] = cachedToken{token: token, expiresAt: time.Now().Add(lifetime)}
	c.tokenMu.Unlock()
	return token, nil
}

func parseAuthChallenge(h string) (realm, service, scope string) {
	h = strings.TrimSpace(h)
	if i := strings.IndexByte(h, ' '); i >= 0 {
		h = h[i+1:]
	}
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		val = strings.Trim(val, `"`)
		switch strings.ToLower(key) {
		case "realm":
			realm = val
		case "service":
			service = val
		case "scope":
			scope = val
		}
	}
	return realm, service, scope
}
