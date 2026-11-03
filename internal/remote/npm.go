package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

const (
	// npmPackumentAccept asks for the abbreviated packument, which holds all an
	// install needs; a registry that ignores it answers with the full document.
	npmPackumentAccept = "application/vnd.npm.install-v1+json, application/json;q=0.8"
	// npmTarballMediaType matches what a hosted publish stores.
	npmTarballMediaType = "application/octet-stream"
	// maxNPMFilenameLength allows npm's longest name and node-semver's longest
	// version with room to spare.
	maxNPMFilenameLength = 512
	maxNPMVersionLength  = 256
)

var (
	// npmFilenamePattern is one path segment with nothing a URL, a header, or a
	// query would need escaped: encodeURIComponent's safe set, which legacy npm
	// names may use, and "+" for build metadata.
	npmFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._~!'()*+-]*\.tgz$`)
	npmVersionPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]*$`)
)

// ValidNPMFilename reports whether a tarball filename is safe to put in a URL, a
// Content-Disposition header, and a storage row. It holds for names upstream
// documents supply and for names a client asks for.
func ValidNPMFilename(name string) bool {
	return len(name) <= maxNPMFilenameLength && npmFilenamePattern.MatchString(name)
}

// NPMVersion is one version of an upstream packument. Only the fields the proxy
// needs are read out; Fields and Dist hold everything as upstream sent it.
type NPMVersion struct {
	Version   string
	Fields    map[string]json.RawMessage // the whole version entry
	Dist      map[string]json.RawMessage // the entry's dist object
	Tarball   string                     // dist.tarball, unchecked until it is fetched
	Filename  string                     // validated basename of Tarball
	Integrity string                     // dist.integrity, may be empty
	Shasum    string                     // dist.shasum, may be empty
}

// NPMPackument is a parsed upstream packument. Keys are matched exactly: Go's
// case-insensitive struct decoding could read a "DIST" npm itself would ignore,
// and then what was verified would not be what the client was told.
//
// A version whose name, filename, or shape Sindri cannot use is left out, and
// any dist-tag naming it goes too. A version that merely has no hash, or a
// tarball on another host, stays listed: that is settled when it is fetched.
type NPMPackument struct {
	Name     string
	Fields   map[string]json.RawMessage // every top-level field as upstream sent it
	DistTags map[string]string
	Versions map[string]NPMVersion
}

// versionWithFilename finds the version whose dist.tarball has this basename. A
// filename is looked up rather than parsed, since name and version do not always
// split cleanly. Two versions claiming one file are refused, because choosing
// between their hashes would depend on map order.
func (p NPMPackument) versionWithFilename(filename string) (NPMVersion, error) {
	var found NPMVersion
	matches := 0
	for _, v := range p.Versions {
		if v.Filename == filename {
			found = v
			matches++
		}
	}
	switch matches {
	case 0:
		return NPMVersion{}, ErrUpstreamNotFound
	case 1:
		return found, nil
	default:
		return NPMVersion{}, fmt.Errorf("%w: %d versions claim one tarball", ErrUpstreamUnavailable, matches)
	}
}

// parseNPMPackument reads an upstream document for name. A document that is not
// an object, or that describes another package, is an upstream failure.
func parseNPMPackument(name string, body []byte) (NPMPackument, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return NPMPackument{}, fmt.Errorf("decoding packument: %w", err)
	}
	var docName string
	if err := json.Unmarshal(fields["name"], &docName); err != nil || docName != name {
		return NPMPackument{}, errors.New("packument does not describe the requested package")
	}
	doc := NPMPackument{Name: name, Fields: fields, DistTags: map[string]string{}, Versions: map[string]NPMVersion{}}

	var entries map[string]json.RawMessage
	if raw, ok := fields["versions"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return NPMPackument{}, fmt.Errorf("decoding packument versions: %w", err)
		}
	}
	for version, raw := range entries {
		if v, ok := parseNPMVersion(version, raw); ok {
			doc.Versions[version] = v
		}
	}

	var tags map[string]string
	if raw, ok := fields["dist-tags"]; ok {
		if err := json.Unmarshal(raw, &tags); err != nil {
			return NPMPackument{}, fmt.Errorf("decoding packument dist-tags: %w", err)
		}
	}
	for tag, version := range tags {
		if _, ok := doc.Versions[version]; ok {
			doc.DistTags[tag] = version
		}
	}
	return doc, nil
}

func parseNPMVersion(version string, raw json.RawMessage) (NPMVersion, bool) {
	if len(version) > maxNPMVersionLength || !npmVersionPattern.MatchString(version) {
		return NPMVersion{}, false
	}
	var fields, dist map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return NPMVersion{}, false
	}
	if err := json.Unmarshal(fields["dist"], &dist); err != nil || dist == nil {
		return NPMVersion{}, false
	}
	var tarball string
	if err := json.Unmarshal(dist["tarball"], &tarball); err != nil {
		return NPMVersion{}, false
	}
	filename, ok := npmTarballBasename(tarball)
	if !ok {
		return NPMVersion{}, false
	}
	// A hash of the wrong type reads as absent, which refuses the tarball.
	var integrity, shasum string
	_ = json.Unmarshal(dist["integrity"], &integrity)
	_ = json.Unmarshal(dist["shasum"], &shasum)
	return NPMVersion{
		Version:   version,
		Fields:    fields,
		Dist:      dist,
		Tarball:   tarball,
		Filename:  filename,
		Integrity: integrity,
		Shasum:    shasum,
	}, true
}

// npmTarballBasename is the last segment of a tarball URL as it was written. An
// escape in it fails validation rather than being decoded, so ..%2f cannot turn
// into a path later.
func npmTarballBasename(tarball string) (string, bool) {
	u, err := url.Parse(tarball)
	if err != nil {
		return "", false
	}
	path := u.EscapedPath()
	name := path[strings.LastIndex(path, "/")+1:]
	return name, ValidNPMFilename(name)
}

// FetchNPMPackument returns a package's packument for a proxy repository, using
// the remote_cache for freshness and negative caching, and serving a stale
// document when upstream is down.
func (c *Client) FetchNPMPackument(ctx context.Context, repoName, name string) (NPMPackument, error) {
	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return NPMPackument{}, err
	}
	key := registry.CacheKeyNPMPackument(name)
	entry, entryErr := c.reg.GetCacheEntry(ctx, repoName, key)

	switch EvaluateCache(entry, time.Now().UTC(), remote.TTL, remote.NegativeTTL, entryErr) {
	case CacheFreshHit:
		return c.loadNPMPackument(ctx, name, entry)
	case CacheFreshNegative:
		return NPMPackument{}, ErrNegativeCached
	case CacheStaleHit:
		doc, err := c.fetchAndCacheNPMPackument(ctx, repoName, name, remote)
		if err == nil {
			return doc, nil
		}
		if !errors.Is(err, ErrUpstreamUnavailable) {
			return NPMPackument{}, err
		}
		return c.loadNPMPackument(ctx, name, entry)
	default:
		return c.fetchAndCacheNPMPackument(ctx, repoName, name, remote)
	}
}

func (c *Client) loadNPMPackument(ctx context.Context, name string, entry registry.CacheEntry) (NPMPackument, error) {
	if entry.BlobDigest == "" {
		return NPMPackument{}, fmt.Errorf("cache hit without metadata blob for %s", entry.Key)
	}
	digest, err := model.ParseDigest(entry.BlobDigest)
	if err != nil {
		return NPMPackument{}, fmt.Errorf("cached packument digest: %w", err)
	}
	body, err := c.reg.ReadBlobBytes(ctx, digest)
	if err != nil {
		return NPMPackument{}, err
	}
	doc, err := parseNPMPackument(name, body)
	if err != nil {
		return NPMPackument{}, fmt.Errorf("cached packument of %s: %w", name, err)
	}
	return doc, nil
}

func (c *Client) fetchAndCacheNPMPackument(ctx context.Context, repoName, name string, remote registry.Remote) (NPMPackument, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	resp, err := c.do(ctx, upstreamRequest{
		method:    http.MethodGet,
		url:       npmPackumentURL(remote.UpstreamURL, name),
		accept:    npmPackumentAccept,
		remote:    remote,
		pinOrigin: true,
	})
	if err != nil {
		return NPMPackument{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	key := registry.CacheKeyNPMPackument(name)
	if resp.StatusCode != http.StatusOK {
		err := classify(resp.StatusCode)
		if errors.Is(err, ErrUpstreamNotFound) {
			if perr := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheMiss, ""); perr != nil {
				return NPMPackument{}, perr
			}
		}
		return NPMPackument{}, err
	}
	body, err := readMetadata(resp.Body)
	if err != nil {
		return NPMPackument{}, err
	}
	// Parsed before it is stored, so a bad document never replaces a good one.
	doc, err := parseNPMPackument(name, body)
	if err != nil {
		return NPMPackument{}, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	}
	digest, err := c.reg.StoreMetadataBlob(ctx, body)
	if err != nil {
		return NPMPackument{}, err
	}
	if err := c.reg.PutCacheEntry(ctx, repoName, key, registry.CacheHit, digest.String()); err != nil {
		return NPMPackument{}, err
	}
	return doc, nil
}

// npmPackumentURL escapes the scope's slash as clients do: @scope%2fname.
func npmPackumentURL(upstream, name string) string {
	base := strings.TrimRight(upstream, "/") + "/"
	scope, rest, scoped := strings.Cut(name, "/")
	if !scoped {
		return base + url.PathEscape(name)
	}
	return base + url.PathEscape(scope) + "%2f" + url.PathEscape(rest)
}

// npmTarballURL returns the URL to fetch a dist.tarball from, or an error if it
// is not on the upstream's host. A hostile packument could otherwise point
// Sindri at an internal address, and integrity is no defence because the same
// document names the hash. Only the path and query of the link are used: scheme
// and host are the upstream's, since registries commonly write http:// links,
// and any userinfo or fragment is dropped.
func npmTarballURL(upstream, tarball string) (string, error) {
	if tarball == "" {
		return "", errors.New("empty tarball URL")
	}
	base, err := url.Parse(strings.TrimRight(upstream, "/") + "/")
	if err != nil {
		return "", fmt.Errorf("parsing upstream URL: %w", err)
	}
	link, err := url.Parse(tarball)
	if err != nil {
		return "", errors.New("unparsable tarball URL")
	}
	u := base.ResolveReference(link)
	if u.Opaque != "" || !strings.EqualFold(comparableHost(u), comparableHost(base)) {
		return "", errors.New("tarball URL is on another host")
	}
	u.Scheme, u.Host, u.User, u.Fragment = base.Scheme, base.Host, nil, ""
	return u.String(), nil
}

// comparableHost is the URL's host without a port that is its own scheme's
// default, so https://h:443 and http://h:80 both read as h.
func comparableHost(u *url.URL) string {
	switch {
	case u.Scheme == "http" && u.Port() == "80", u.Scheme == "https" && u.Port() == "443":
		return u.Hostname()
	default:
		return u.Host
	}
}

// FetchNPMTarball returns a tarball from a proxy repository. A placed copy is
// served through the placement check without contacting upstream. Otherwise the
// file is found in the packument, and streams back while it is staged; it is
// placed only once it matches what the packument promised, and a mismatch fails
// the final read.
func (c *Client) FetchNPMTarball(ctx context.Context, repoName, name, filename string) (registry.PackageFile, io.ReadCloser, error) {
	if file, rc, err := c.reg.GetNPMTarball(ctx, repoName, name, filename); err == nil {
		return file, rc, nil
	} else if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced) {
		return registry.PackageFile{}, nil, err
	}

	doc, err := c.FetchNPMPackument(ctx, repoName, name)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	version, err := doc.versionWithFilename(filename)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	check, ok := newIntegrityCheck(version.Integrity, version.Shasum)
	if !ok {
		c.log.WarnContext(ctx, "npm proxy refused a version with no usable hash",
			slog.String("repository", repoName), slog.String("package", name), slog.String("version", version.Version))
		return registry.PackageFile{}, nil, fmt.Errorf("%w: %s %s has no usable hash", ErrUpstreamUnavailable, name, version.Version)
	}
	fileURL, err := npmTarballURL(remote.UpstreamURL, version.Tarball)
	if err != nil {
		c.log.WarnContext(ctx, "npm proxy refused a tarball URL",
			slog.String("repository", repoName), slog.String("package", name), slog.String("version", version.Version), slog.Any("error", err))
		return registry.PackageFile{}, nil, fmt.Errorf("%w: %s %s: %w", ErrUpstreamUnavailable, name, version.Version, err)
	}
	meta, err := json.Marshal(map[string]string{"name": name, "version": version.Version})
	if err != nil {
		return registry.PackageFile{}, nil, fmt.Errorf("encoding cached file metadata: %w", err)
	}

	resp, streamCtx, cancel, idle, err := c.openStream(ctx, upstreamRequest{
		method:    http.MethodGet,
		url:       fileURL,
		remote:    remote,
		pinOrigin: true,
	})
	if err != nil {
		return registry.PackageFile{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		idle.Stop()
		cancel()
		return registry.PackageFile{}, nil, classify(resp.StatusCode)
	}

	body := c.newCachingBody(streamCtx, resp, cancel, idle, model.Digest{}, func(ctx context.Context, info storage.BlobInfo) error {
		_, err := c.reg.PlaceCachedFile(ctx, registry.PlaceCachedFileParams{
			Repo:      repoName,
			Package:   name,
			Version:   version.Version,
			Filename:  filename,
			MediaType: npmTarballMediaType,
			Metadata:  meta,
			Blob:      info,
		})
		return err
	})
	body.integrity = check
	return registry.PackageFile{
		Filename:  filename,
		MediaType: npmTarballMediaType,
		Size:      resp.ContentLength,
		Version:   version.Version,
	}, body, nil
}
