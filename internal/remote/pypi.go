package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

// PyPIIndexFile is one file listed by an upstream PEP 691 simple index.
type PyPIIndexFile struct {
	Filename string
	URL      string
	Hashes   map[string]string
	Requires string
	Size     int64
	Yanked   bool
}

// PyPIIndex is a parsed upstream project page.
type PyPIIndex struct {
	Name     string
	Files    []PyPIIndexFile
	Versions []string
	Body     []byte // original upstream JSON, stored on cache hit
}

// Filenames are the filenames the index lists.
func (i PyPIIndex) Filenames() []string {
	names := make([]string, 0, len(i.Files))
	for _, f := range i.Files {
		names = append(names, f.Filename)
	}
	return names
}

// FetchPyPIIndex returns the project index for a proxy repository, using the
// remote_cache for freshness and negative caching.
func (c *Client) FetchPyPIIndex(ctx context.Context, repoName, project string) (PyPIIndex, error) {
	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return PyPIIndex{}, err
	}
	key := registry.CacheKeyPyPIIndex(project)
	now := c.now().UTC()
	entry, entryErr := c.reg.GetCacheEntry(ctx, repoName, key)
	decision := EvaluateCache(entry, now, remote.TTL, remote.NegativeTTL, entryErr)

	switch decision {
	case CacheFreshHit:
		return c.loadPyPIIndex(ctx, entry)
	case CacheFreshNegative:
		return PyPIIndex{}, ErrNegativeCached
	case CacheStaleHit:
		if c.inBackoff(repoName) {
			return c.loadPyPIIndex(ctx, entry)
		}
		idx, err := c.refreshPyPIIndex(ctx, repoName, project, remote)
		if err == nil {
			return idx, nil
		}
		if errors.Is(err, ErrUpstreamNotFound) {
			return PyPIIndex{}, err
		}
		// Upstream down: serve stale.
		return c.loadPyPIIndex(ctx, entry)
	default:
		if c.inBackoff(repoName) {
			return PyPIIndex{}, errBackingOff(repoName)
		}
		return c.refreshPyPIIndex(ctx, repoName, project, remote)
	}
}

// refreshPyPIIndex fetches and caches an index, and tells the backoff what that
// showed about the upstream.
func (c *Client) refreshPyPIIndex(ctx context.Context, repoName, project string, remote registry.Remote) (PyPIIndex, error) {
	idx, err := c.fetchAndCachePyPIIndex(ctx, repoName, project, remote)
	c.noteRefresh(repoName, err)
	return idx, err
}

func (c *Client) loadPyPIIndex(ctx context.Context, entry registry.CacheEntry) (PyPIIndex, error) {
	if entry.BlobDigest == "" {
		return PyPIIndex{}, fmt.Errorf("cache hit without metadata blob for %s", entry.Key)
	}
	digest, err := model.ParseDigest(entry.BlobDigest)
	if err != nil {
		return PyPIIndex{}, fmt.Errorf("cached index digest: %w", err)
	}
	body, err := c.reg.ReadBlobBytes(ctx, digest)
	if err != nil {
		return PyPIIndex{}, err
	}
	return parsePyPIIndex(body)
}

func (c *Client) fetchAndCachePyPIIndex(ctx context.Context, repoName, project string, remote registry.Remote) (PyPIIndex, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	resp, err := c.do(ctx, upstreamRequest{
		method: http.MethodGet,
		url:    strings.TrimRight(remote.UpstreamURL, "/") + "/" + project + "/",
		accept: "application/vnd.pypi.simple.v1+json,application/json;q=0.9",
		remote: remote,
		repo:   repoName,
	})
	if err != nil {
		return PyPIIndex{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	key := registry.CacheKeyPyPIIndex(project)
	if resp.StatusCode != http.StatusOK {
		err := c.refusal(ctx, repoName, resp.StatusCode, classify(resp.StatusCode))
		if errors.Is(err, ErrUpstreamNotFound) {
			// Upstream answered that the project is gone: so are its files.
			if serr := c.reg.SyncProxyFiles(ctx, repoName, registry.NormalizeName(model.ProtocolPyPI, project), nil); serr != nil {
				return PyPIIndex{}, serr
			}
			if perr := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheMiss, ""); perr != nil {
				return PyPIIndex{}, perr
			}
		}
		return PyPIIndex{}, err
	}
	body, err := readMetadata(resp.Body, maxMetadataBytes)
	if err != nil {
		return PyPIIndex{}, err
	}

	idx, err := parsePyPIIndex(body)
	if err == nil {
		err = checkPyPIIndex(body, project)
	}
	if err != nil {
		return PyPIIndex{}, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	}
	digest, err := c.reg.StoreMetadataBlob(ctx, body)
	if err != nil {
		return PyPIIndex{}, err
	}
	// Before the entry is stored: if this fails the entry stays stale, so the
	// next read refreshes and records it again.
	if err := c.reg.SyncProxyFiles(ctx, repoName, registry.NormalizeName(model.ProtocolPyPI, project), idx.Filenames()); err != nil {
		return PyPIIndex{}, err
	}
	if err := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheHit, digest.String()); err != nil {
		return PyPIIndex{}, err
	}
	idx.Body = body
	return idx, nil
}

// checkPyPIIndex refuses a 200 that is not this project's index, before it can
// say anything about which files exist. PEP 691 requires "files" and "name" in
// a project detail. "files" must be present, though an empty list is a real
// answer that upstream lists nothing. "name" is compared only when present, so
// a mirror that leaves it out is still served; a name that normalizes to
// another project is refused (SIN-54).
func checkPyPIIndex(body []byte, project string) error {
	var raw struct {
		Name  *string         `json:"name"`
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decoding simple JSON: %w", err)
	}
	if len(raw.Files) == 0 || string(raw.Files) == "null" {
		return errors.New("simple JSON has no files list")
	}
	if raw.Name != nil && registry.NormalizeName(model.ProtocolPyPI, *raw.Name) != registry.NormalizeName(model.ProtocolPyPI, project) {
		return fmt.Errorf("simple JSON is for project %q, not %q", *raw.Name, project)
	}
	return nil
}

func parsePyPIIndex(body []byte) (PyPIIndex, error) {
	var raw struct {
		Name     string   `json:"name"`
		Versions []string `json:"versions"`
		Files    []struct {
			Filename string            `json:"filename"`
			URL      string            `json:"url"`
			Hashes   map[string]string `json:"hashes"`
			Size     *int64            `json:"size"`
			Requires *string           `json:"requires-python"`
			Yanked   any               `json:"yanked"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PyPIIndex{}, fmt.Errorf("decoding simple JSON: %w", err)
	}
	idx := PyPIIndex{Name: raw.Name, Versions: raw.Versions, Body: body}
	for _, f := range raw.Files {
		req := ""
		if f.Requires != nil {
			req = *f.Requires
		}
		yanked := false
		switch v := f.Yanked.(type) {
		case bool:
			yanked = v
		case string:
			yanked = v != ""
		}
		// size is optional before api-version 1.1; -1 means unknown.
		size := int64(-1)
		if f.Size != nil {
			size = *f.Size
		}
		idx.Files = append(idx.Files, PyPIIndexFile{
			Filename: f.Filename,
			URL:      f.URL,
			Hashes:   f.Hashes,
			Requires: req,
			Size:     size,
			Yanked:   yanked,
		})
	}
	return idx, nil
}

// FetchPyPIFile returns one file from a proxy repository. On a miss the
// upstream body streams straight back while it is staged, and is placed once
// its digest verifies against expectedDigest (when the index gave one). size
// is the index's claim; a negative value defers to upstream's Content-Length,
// and the returned Size is -1 when neither says.
func (c *Client) FetchPyPIFile(ctx context.Context, repoName, project, version, filename, fileURL string, expectedDigest model.Digest, size int64) (registry.PackageFile, io.ReadCloser, error) {
	// A placed copy is the caller's to serve first: GetPyPIFile fetches the
	// index before this, so a file upstream dropped is already flagged and
	// invisible to this read.
	if file, rc, err := c.reg.GetFileByFilename(ctx, repoName, project, filename); err == nil {
		return file, rc, nil
	} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		return registry.PackageFile{}, nil, err
	}

	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	if fileURL == "" {
		return registry.PackageFile{}, nil, fmt.Errorf("%w: empty upstream file URL", ErrUpstreamUnavailable)
	}
	meta, err := json.Marshal(map[string]any{"name": project, "version": version})
	if err != nil {
		return registry.PackageFile{}, nil, fmt.Errorf("encoding cached file metadata: %w", err)
	}

	resp, streamCtx, cancel, idle, err := c.openStream(ctx, upstreamRequest{
		method: http.MethodGet,
		url:    fileURL,
		remote: remote,
		repo:   repoName,
	})
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		idle.Stop()
		cancel()
		return registry.PackageFile{}, nil, c.refusal(ctx, repoName, resp.StatusCode, classify(resp.StatusCode))
	}

	body := c.newCachingBody(streamCtx, resp, cancel, idle, expectedDigest, func(ctx context.Context, info storage.BlobInfo) error {
		_, err := c.reg.PlaceCachedFile(ctx, registry.PlaceCachedFileParams{
			Repo:      repoName,
			Package:   project,
			Version:   version,
			Filename:  filename,
			MediaType: "application/octet-stream",
			Metadata:  meta,
			Blob:      info,
		})
		return err
	})
	switch {
	case size < 0:
		size = resp.ContentLength
	case resp.ContentLength >= 0 && resp.ContentLength != size:
		_ = body.Close()
		return registry.PackageFile{}, nil, fmt.Errorf("%w: index says %d bytes, upstream sends %d",
			ErrUpstreamUnavailable, size, resp.ContentLength)
	}
	body.size = size
	return registry.PackageFile{
		Digest:    expectedDigest,
		Filename:  filename,
		MediaType: "application/octet-stream",
		Size:      size,
		Version:   version,
	}, body, nil
}

// ResolveFileURL returns an absolute upstream URL for a file, resolving
// relatives against the upstream base.
func ResolveFileURL(upstreamBase, fileURL string) (string, error) {
	if fileURL == "" {
		return "", errors.New("empty file URL")
	}
	u, err := url.Parse(fileURL)
	if err != nil {
		return "", fmt.Errorf("parsing file URL: %w", err)
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(strings.TrimRight(upstreamBase, "/") + "/")
	if err != nil {
		return "", fmt.Errorf("parsing upstream URL: %w", err)
	}
	return base.ResolveReference(u).String(), nil
}
