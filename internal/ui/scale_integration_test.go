//go:build integration

package ui

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/testsupport"
)

// The scale tests render every kind of page against testsupport.SeedLarge: 500
// repositories, 50,000 packages, and lifecycle, evidence, and scan history on
// top. They guard two things that a page with ten rows never shows: that a
// page is quick, and that the number of queries it makes does not grow with
// the data (an N+1 is invisible at ten rows and a stall at five hundred).

// queryCounter is a pgx.QueryTracer that counts the queries started.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (*queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type scalePage struct {
	name string
	user string
	path string
	// maxQueries is the ceiling on queries for one request. It is a constant:
	// a page may query more than once, but never once per row it lists.
	maxQueries int64
}

func scalePages(info testsupport.SeedInfo) []scalePage {
	pkg := func(repo, name string) string {
		return "/ui/r/" + repo + "/-/package?" + url.Values{"name": {name}}.Encode()
	}
	artifact := func(repo, image, digest string) string {
		q := url.Values{"digest": {digest}}
		if image != "" {
			q.Set("image", image)
		}
		return "/ui/r/" + repo + "/-/artifact?" + q.Encode()
	}
	return []scalePage{
		{"repositories", "admin", "/ui/", 6},
		{"repositories, filtered by text and type", "admin", "/ui/?q=oci-00&type=hosted", 6},
		{"repositories, a later page", "admin", "/ui/?page=3", 6},
		{"repositories, table view", "admin", "/ui/?view=table", 6},
		{"repositories, tiles of the first page", "admin", "/ui/?view=tiles", 6},
		{"hosted repository", "admin", "/ui/r/" + info.HostedOCI, 8},
		{"hosted repository, filtered and sorted", "admin", "/ui/r/" + info.HostedOCI + "?q=pkg-00&sort=published", 8},
		{"hosted repository, a later page", "admin", "/ui/r/" + info.HostedOCI + "?page=3", 8},
		{"hosted pypi repository", "admin", "/ui/r/" + info.PyPIRepo, 8},
		{"hosted npm repository", "admin", "/ui/r/" + info.NPMRepo, 9},
		{"proxy repository", "admin", "/ui/r/" + info.ProxyOCI, 7},
		{"virtual repository", "admin", "/ui/r/" + info.VirtualOCI, 4},
		{"oci package", "admin", pkg(info.HostedOCI, info.OCIImage), 6},
		{"oci package of 2,000 tags", "admin", pkg(info.HostedOCI, info.HugeTagImage), 6},
		{"oci package of 2,000 tags, a later page", "admin", pkg(info.HostedOCI, info.HugeTagImage) + "&cursor=v1.1000", 6},
		{"pypi package", "admin", pkg(info.PyPIRepo, info.PyPIPackage), 6},
		{"npm package", "admin", pkg(info.NPMRepo, info.NPMPackage), 8},
		{"oci artifact", "admin", artifact(info.HostedOCI, info.OCIImage, info.OCIDigest), 22},
		{"pypi artifact", "admin", artifact(info.PyPIRepo, info.PyPIPackage, info.PyPIDigest), 19},
		{"npm artifact", "admin", artifact(info.NPMRepo, info.NPMPackage, info.NPMDigest), 21},
		// A caller who is not an administrator goes through the readability
		// filters, which an administrator skips: alice reads every seeded
		// repository but the restricted ones, and seeder may also write.
		{"repositories, filtered", "alice", "/ui/", 6},
		{"repositories, filtered by text and type, as alice", "alice", "/ui/?q=oci-00&type=hosted", 6},
		{"repositories, a later page, as alice", "alice", "/ui/?page=2&view=table", 6},
		{"hosted repository, filtered", "alice", "/ui/r/" + info.HostedOCI, 8},
		{"virtual repository, filtered", "alice", "/ui/r/" + info.VirtualOCI, 4},
		{"oci package, filtered", "alice", pkg(info.HostedOCI, info.OCIImage), 6},
		{"oci package of 2,000 tags, as alice", "alice", pkg(info.HostedOCI, info.HugeTagImage), 6},
		{"oci artifact, filtered", "alice", artifact(info.HostedOCI, info.OCIImage, info.OCIDigest), 21},
		{"oci artifact, writer", "seeder", artifact(info.HostedOCI, info.OCIImage, info.OCIDigest), 21},
		{"pypi artifact, writer", "seeder", artifact(info.PyPIRepo, info.PyPIPackage, info.PyPIDigest), 20},
		{"system", "admin", "/ui/system", 3},
		{"notifications", "admin", "/ui/system/notifications", 3},
	}
}

func seededEnv(t *testing.T) (*env, *queryCounter, testsupport.SeedInfo) {
	t.Helper()
	counter := &queryCounter{}
	e := newEnvTraced(t, counter)
	start := time.Now()
	info := testsupport.SeedLarge(t, e.pool)
	t.Logf("seeded in %s", time.Since(start).Round(time.Millisecond))
	return e, counter, info
}

func TestScale_PagesRenderWithinTheBudget(t *testing.T) {
	const (
		requests = 20
		budget   = 200 * time.Millisecond
	)
	e, _, info := seededEnv(t)
	for _, pg := range scalePages(info) {
		t.Run(pg.name, func(t *testing.T) {
			if r := e.get(pg.user, pg.path); r.status != http.StatusOK { // warm-up, and a check the page exists
				t.Fatalf("GET %s: %d", pg.path, r.status)
			}
			times := make([]time.Duration, 0, requests)
			for range requests {
				start := time.Now()
				if r := e.get(pg.user, pg.path); r.status != http.StatusOK {
					t.Fatalf("GET %s: %d", pg.path, r.status)
				}
				times = append(times, time.Since(start))
			}
			slices.Sort(times)
			p95 := times[(requests*95+99)/100-1]
			t.Logf("p95 %s (median %s, max %s)", p95.Round(time.Microsecond), times[requests/2].Round(time.Microsecond), times[requests-1].Round(time.Microsecond))
			if p95 > budget {
				t.Errorf("p95 of %s is %s, over the %s budget", pg.path, p95, budget)
			}
		})
	}
}

func TestScale_QueryCountDoesNotGrowWithRows(t *testing.T) {
	e, counter, info := seededEnv(t)
	for _, pg := range scalePages(info) {
		t.Run(pg.name, func(t *testing.T) {
			if pg.maxQueries == 0 {
				t.Fatalf("no query ceiling set for %s", pg.name)
			}
			before := counter.n.Load()
			if r := e.get(pg.user, pg.path); r.status != http.StatusOK {
				t.Fatalf("GET %s: %d", pg.path, r.status)
			}
			got := counter.n.Load() - before
			t.Logf("%d queries (ceiling %d)", got, pg.maxQueries)
			if got > pg.maxQueries {
				t.Errorf("%s made %d queries, ceiling %d: a page's queries must not grow with the rows it lists", pg.path, got, pg.maxQueries)
			}
		})
	}
}

// The non-administrator rows above only measure the filtered paths if the
// filters are really in play: alice must not see a restricted repository,
// and seeder must be offered the Manage commands a writer gets.
func TestScale_NonAdminRowsExerciseTheFilters(t *testing.T) {
	e, _, info := seededEnv(t)
	restricted := "seed/oci-0005" // hosted, and every fifth hosted repository is restricted
	if r := e.get("admin", "/ui/"); !strings.Contains(r.body, restricted) {
		t.Fatalf("admin's list does not name %s; the seed's restricted repositories moved", restricted)
	}
	if r := e.get("alice", "/ui/"); strings.Contains(r.body, restricted) {
		t.Errorf("alice's list names the restricted repository %s", restricted)
	}
	page := "/ui/r/" + info.HostedOCI + "/-/artifact?" + url.Values{"digest": {info.OCIDigest}, "image": {info.OCIImage}}.Encode()
	if r := e.get("seeder", page); !strings.Contains(r.body, "/promote") && !strings.Contains(r.body, "/revoke") {
		t.Error("seeder's artifact page offers no promote or revoke command")
	}
	if r := e.get("alice", page); strings.Contains(r.body, "/promote") || strings.Contains(r.body, "/revoke") {
		t.Error("alice, who may not write, is offered a promote or revoke command")
	}
}
