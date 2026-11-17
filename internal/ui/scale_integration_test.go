//go:build integration

package ui

import (
	"context"
	"net/http"
	"net/url"
	"slices"
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
		{"repositories", "admin", "/ui/", 5},
		{"hosted repository", "admin", "/ui/r/" + info.HostedOCI, 5},
		{"proxy repository", "admin", "/ui/r/" + info.ProxyOCI, 6},
		{"virtual repository", "admin", "/ui/r/" + info.VirtualOCI, 4},
		{"oci package", "admin", pkg(info.HostedOCI, info.OCIImage), 5},
		{"pypi package", "admin", pkg(info.PyPIRepo, info.PyPIPackage), 5},
		{"npm package", "admin", pkg(info.NPMRepo, info.NPMPackage), 7},
		{"oci artifact", "admin", artifact(info.HostedOCI, info.OCIImage, info.OCIDigest), 22},
		{"pypi artifact", "admin", artifact(info.PyPIRepo, info.PyPIPackage, info.PyPIDigest), 19},
		{"npm artifact", "admin", artifact(info.NPMRepo, info.NPMPackage, info.NPMDigest), 21},
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
