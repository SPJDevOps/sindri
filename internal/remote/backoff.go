package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// refreshBackoff is how long a repository's package documents are not refreshed
// after a refresh found the upstream not answering (SIN-61). Without it, every
// read past the TTL waits the whole upstream timeout during an outage, since a
// placed file is only served once its document has been refreshed. A constant,
// not configuration: it only has to outlast a burst of requests and end soon
// after an upstream recovers.
const refreshBackoff = 30 * time.Second

// errUpstreamDown marks a failure to get an answer from the upstream: a
// connection that failed, or a timeout before the response headers. It is what
// starts a backoff, and nothing else does. A 5xx is an answer, and a fast one,
// so it fails its own request and costs the next no more. ErrUpstreamUnavailable
// is wider, since a 5xx, an oversize or unparsable document and a 401/403 wrap
// it too, and one package's bad document says nothing about the other packages
// of the repository.
var errUpstreamDown = errors.New("upstream not answering")

// ErrFileGone means a refresh showed that upstream no longer lists the file. It
// is an upstream not-found, so a virtual walk treats it as absence.
var ErrFileGone = fmt.Errorf("%w: upstream no longer lists the file", ErrUpstreamNotFound)

// down marks err as the upstream not answering, unless the caller or a
// redirect rule ended the request: those say nothing about the upstream.
func down(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, errRedirectRefused) {
		return err
	}
	return fmt.Errorf("%w: %w", errUpstreamDown, err)
}

// inBackoff reports whether a refresh of repo's documents failed recently.
func (c *Client) inBackoff(repo string) bool {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	until, ok := c.backoffUntil[repo]
	return ok && c.now().Before(until)
}

// noteRefresh records what a document refresh learned about the upstream. Success
// or a 404 ends a backoff; an outage (no answer) starts one; any other
// outcome (a 5xx, 401, 403, 429, a bad document) changes nothing.
func (c *Client) noteRefresh(repo string, err error) {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	switch {
	case err == nil || errors.Is(err, ErrUpstreamNotFound):
		delete(c.backoffUntil, repo)
	case errors.Is(err, errUpstreamDown):
		c.backoffUntil[repo] = c.now().Add(refreshBackoff)
	}
}

// errBackingOff is what a read with nothing local to serve gets during a
// backoff. It is an ordinary unavailable upstream to every caller.
func errBackingOff(repo string) error {
	return fmt.Errorf("%w: %s failed to answer recently, not asking again yet", ErrUpstreamUnavailable, repo)
}

// docState is how much of a read local state can settle without the document.
type docState int

const (
	// stateRefresh: the document must be consulted, and refreshed if stale.
	stateRefresh docState = iota
	// stateFresh: the document is fresh, so the flag on a file, set by the
	// refresh that stored it, is authoritative and the document is not parsed.
	stateFresh
	// stateBackoff: the document is stale and upstream is not being asked.
	stateBackoff
)

func (c *Client) stateOf(ctx context.Context, repoName, key string) (docState, error) {
	remote, err := c.reg.GetRemote(ctx, repoName)
	if err != nil {
		return stateRefresh, err
	}
	entry, entryErr := c.reg.GetCacheEntry(ctx, repoName, key)
	if errors.Is(entryErr, registry.ErrNotFound) {
		return stateRefresh, nil //nolint:nilerr // no cache entry is an ordinary miss, not a failure
	}
	if entryErr != nil {
		return stateRefresh, entryErr
	}
	if entry.Status != registry.CacheHit {
		return stateRefresh, nil
	}
	switch EvaluateCache(entry, c.now().UTC(), remote.TTL, remote.NegativeTTL, nil) {
	case CacheFreshHit:
		return stateFresh, nil
	case CacheStaleHit:
		if c.inBackoff(repoName) {
			return stateBackoff, nil
		}
	case CacheMiss, CacheFreshNegative:
	}
	return stateRefresh, nil
}

// servePlaced answers a file read from local state alone when that is enough,
// and reports done=false when the document has to be consulted. With a fresh
// document or a backoff, a placed copy serves (get checks placement, I2), a
// file flagged as gone is ErrFileGone, and during a backoff anything else is
// unavailable at once. On a fresh document a file that is neither placed nor
// flagged still needs the document, to find where it lives.
func (c *Client) servePlaced(ctx context.Context, repoName, key, normalized, filename string,
	get func(context.Context) (registry.PackageFile, io.ReadCloser, error),
) (registry.PackageFile, io.ReadCloser, bool, error) {
	state, err := c.stateOf(ctx, repoName, key)
	if err != nil {
		return registry.PackageFile{}, nil, true, err
	}
	if state == stateRefresh {
		return registry.PackageFile{}, nil, false, nil
	}
	file, rc, err := get(ctx)
	switch {
	case err == nil:
		return file, rc, true, nil
	case !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPlaced):
		return registry.PackageFile{}, nil, true, err
	}
	gone, err := c.reg.ProxyFileGone(ctx, repoName, normalized, filename)
	switch {
	case err != nil:
		return registry.PackageFile{}, nil, true, err
	case gone:
		return registry.PackageFile{}, nil, true, ErrFileGone
	case state == stateBackoff:
		return registry.PackageFile{}, nil, true, errBackingOff(repoName)
	}
	return registry.PackageFile{}, nil, false, nil
}

// ServePlacedPyPIFile is servePlaced for a file of a PyPI proxy repository.
func (c *Client) ServePlacedPyPIFile(ctx context.Context, repoName, project, filename string) (registry.PackageFile, io.ReadCloser, bool, error) {
	return c.servePlaced(ctx, repoName, registry.CacheKeyPyPIIndex(project),
		registry.NormalizeName(model.ProtocolPyPI, project), filename,
		func(ctx context.Context) (registry.PackageFile, io.ReadCloser, error) {
			return c.reg.GetFileByFilename(ctx, repoName, project, filename)
		})
}

// ServePlacedNPMTarball is servePlaced for a tarball of an npm proxy repository.
func (c *Client) ServePlacedNPMTarball(ctx context.Context, repoName, name, filename string) (registry.PackageFile, io.ReadCloser, bool, error) {
	return c.servePlaced(ctx, repoName, registry.CacheKeyNPMPackument(name),
		registry.NormalizeName(model.ProtocolNPM, name), filename,
		func(ctx context.Context) (registry.PackageFile, io.ReadCloser, error) {
			return c.reg.GetNPMTarball(ctx, repoName, name, filename)
		})
}
