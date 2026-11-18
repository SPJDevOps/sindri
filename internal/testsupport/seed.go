//go:build integration

package testsupport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAlreadySeeded is what Seed returns on a database that holds a seed
// already. The seed is not additive: a second run would collide on its own
// names, so it refuses rather than half-apply.
var ErrAlreadySeeded = errors.New("the database already holds the large seed (repositories named seed/...)")

// SeedInfo names rows of the large seed that tests can address. Every name is
// deterministic, so the same seed always produces the same ones.
type SeedInfo struct {
	// Totals, as written.
	Repositories int
	Packages     int
	Artifacts    int
	Blobs        int
	Subjects     int
	Evidence     int
	Events       int

	// Repositories by type, for the OCI protocol.
	HostedOCI  string
	ProxyOCI   string
	VirtualOCI string // members: two hosted repositories and one proxy

	// One artifact per protocol that carries lifecycle history, evidence, and
	// (OCI) scan findings. The digest is as placed in the matching hosted
	// repository.
	OCIImage  string
	OCITag    string
	OCIDigest string

	PyPIRepo    string
	PyPIPackage string
	PyPIDigest  string

	NPMRepo    string
	NPMPackage string
	NPMDigest  string

	// Tags on OCIImage in HostedOCI, more than one page of them.
	OCITags int
	// HugeTagImage is a second image of HostedOCI with HugeTags tags, each its
	// own manifest with an identity, most with scans and approvals: the shape
	// of a busy CI repository, for the pages that list tags and for the
	// repository page that summarises every tag of an image.
	HugeTagImage string
	HugeTags     int
	// ManyImagesRepo is a hosted OCI repository whose first ManyImages images
	// each have ManyImagesTags tags: the shape that makes a repository page's
	// cost depend on tags, not on the rows it lists.
	ManyImagesRepo string
	ManyImages     int
	ManyImagesTags int
	// Versions of PyPIPackage and NPMPackage, which are deliberately large.
	HeavyVersions int
}

// Seed shape. Hosted repositories carry most packages, as they do in practice.
const (
	seedHostedPerProtocol  = 100
	seedHostedPackages     = 150
	seedProxyPackages      = 50
	seedHeavyVersions      = 120
	seedHeavyTags          = 60
	seedHugeTags           = 2000
	seedManyImages         = 50
	seedManyImagesTags     = 200
	seedKnownScanFindings  = 300
	seedSubjectEveryNth    = 4
	seedRevokeEveryNth     = 7
	seedRestrictedEveryNth = 5
)

// seedProtocols lists, per protocol, how many proxy and virtual repositories
// to create. With seedHostedPerProtocol hosted each, the total is 500.
var seedProtocols = []struct {
	name    string
	proxies int
	virtual int
}{
	{"oci", 34, 33},
	{"pypi", 33, 33},
	{"npm", 33, 34},
}

// SeedLarge fills a migrated database with a registry of realistic size: 500
// repositories (hosted, proxy, and virtual across oci, pypi, and npm), 50,000
// packages, their artifacts and placements, lifecycle history, evidence, and
// scan results with findings. It writes rows directly with COPY, never through
// the services, so it takes seconds; no bytes exist in any object store, which
// the UI does not need to render.
func SeedLarge(t testing.TB, pool *pgxpool.Pool) SeedInfo {
	t.Helper()
	info, err := Seed(t.Context(), pool)
	if err != nil {
		t.Fatalf("seeding the database: %v", err)
	}
	return info
}

// Seed is SeedLarge for callers that are not a test, with an error in place
// of a fatal one. It runs in one transaction: it applies completely or not at
// all.
func Seed(ctx context.Context, pool *pgxpool.Pool) (SeedInfo, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return SeedInfo{}, fmt.Errorf("beginning the seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name LIKE 'seed/%')`).Scan(&exists); err != nil {
		return SeedInfo{}, fmt.Errorf("checking for an earlier seed: %w", err)
	}
	if exists {
		return SeedInfo{}, ErrAlreadySeeded
	}

	s := &seeder{now: time.Now().UTC().Truncate(time.Second)}
	if err := s.loadStages(ctx, tx); err != nil {
		return SeedInfo{}, err
	}
	s.generate()
	if err := s.copyAll(ctx, tx); err != nil {
		return SeedInfo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SeedInfo{}, fmt.Errorf("committing the seed: %w", err)
	}
	// A bulk load leaves the planner without statistics, so it plans the way it
	// would for empty tables until autovacuum catches up, a minute or so later.
	// Analyzing now makes a measurement of the seeded set the same as one of a
	// registry that has been running for a while.
	if _, err := pool.Exec(ctx, "ANALYZE"); err != nil {
		return SeedInfo{}, fmt.Errorf("analyzing the seeded tables: %w", err)
	}
	return s.info, nil
}

type stage struct {
	id       uuid.UUID
	name     string
	position int
}

type seedRepo struct {
	id       uuid.UUID
	name     string
	typ      string
	protocol string
	index    int // 1-based within its protocol and type
}

type seeder struct {
	now    time.Time
	stages []stage
	info   SeedInfo

	repos      [][]any
	remotes    [][]any
	members    [][]any
	blobs      [][]any
	repoBlobs  [][]any
	packages   [][]any
	versions   [][]any
	files      [][]any
	tags       [][]any
	distTags   [][]any
	subjects   [][]any
	owners     [][]any
	provenance [][]any
	evidence   [][]any
	events     [][]any
	stageRows  [][]any

	blobSeen map[string]bool
	artifact int // running artifact counter, drives the subject mix
}

var seedNS = uuid.MustParse("5d1d7f0e-5a0c-4a7e-9c11-6e5d0f2b7a10")

func seedID(parts ...string) uuid.UUID {
	key := ""
	for _, p := range parts {
		key += p + "\x00"
	}
	return uuid.NewSHA1(seedNS, []byte(key))
}

func seedDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// loadStages reads the stages the database has, and creates dev, staging, and
// prod when it has none, so lifecycle history has stages to refer to.
func (s *seeder) loadStages(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT id, name, position FROM lifecycle_stages ORDER BY position`)
	if err != nil {
		return fmt.Errorf("reading lifecycle stages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st stage
		if err := rows.Scan(&st.id, &st.name, &st.position); err != nil {
			return fmt.Errorf("scanning a lifecycle stage: %w", err)
		}
		s.stages = append(s.stages, st)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading lifecycle stages: %w", err)
	}
	if len(s.stages) > 0 {
		return nil
	}
	for i, name := range []string{"dev", "staging", "prod"} {
		st := stage{id: seedID("stage", name), name: name, position: i}
		s.stages = append(s.stages, st)
		s.stageRows = append(s.stageRows, []any{st.id, st.name, st.position})
	}
	return nil
}

func repoName(protocol, typ string, i int) string {
	switch typ {
	case "proxy":
		return fmt.Sprintf("seed/%s-proxy-%04d", protocol, i)
	case "virtual":
		return fmt.Sprintf("seed/%s-all-%04d", protocol, i)
	default:
		return fmt.Sprintf("seed/%s-%04d", protocol, i)
	}
}

func upstreamFor(protocol string, i int) string {
	switch protocol {
	case "pypi":
		return fmt.Sprintf("https://pypi-mirror-%d.example.com/simple/", i)
	case "npm":
		return fmt.Sprintf("https://npm-mirror-%d.example.com", i)
	default:
		return fmt.Sprintf("https://registry-%d.example.com", i)
	}
}

func (s *seeder) generate() {
	s.blobSeen = map[string]bool{}
	hosted := map[string][]seedRepo{}
	proxies := map[string][]seedRepo{}

	add := func(protocol, typ string, i int) seedRepo {
		r := seedRepo{id: seedID("repo", repoName(protocol, typ, i)), name: repoName(protocol, typ, i), typ: typ, protocol: protocol, index: i}
		read := `["authenticated"]`
		if typ == "hosted" && i%seedRestrictedEveryNth == 0 {
			read = `["team:platform"]`
		}
		cfg := fmt.Sprintf(`{"access":{"read":%s,"write":["team:seed"]}}`, read)
		s.repos = append(s.repos, []any{r.id, r.name, r.typ, r.protocol, []byte(cfg), typ == "hosted" && i%25 == 0, "api"})
		return r
	}

	for _, p := range seedProtocols {
		for i := 1; i <= seedHostedPerProtocol; i++ {
			hosted[p.name] = append(hosted[p.name], add(p.name, "hosted", i))
		}
		for i := 1; i <= p.proxies; i++ {
			r := add(p.name, "proxy", i)
			proxies[p.name] = append(proxies[p.name], r)
			s.remotes = append(s.remotes, []any{r.id, upstreamFor(p.name, i), 300, 60})
		}
		for i := 1; i <= p.virtual; i++ {
			v := add(p.name, "virtual", i)
			h := hosted[p.name]
			px := proxies[p.name]
			for pos, m := range []seedRepo{h[i-1], h[i%len(h)], px[(i-1)%len(px)]} {
				s.members = append(s.members, []any{v.id, m.id, pos})
			}
		}
	}
	s.info.Repositories = len(s.repos)
	s.info.HostedOCI = repoName("oci", "hosted", 1)
	s.info.ProxyOCI = repoName("oci", "proxy", 1)
	s.info.VirtualOCI = repoName("oci", "virtual", 1)
	s.info.OCIImage, s.info.OCITag = "pkg-0001", "latest"
	s.info.PyPIRepo, s.info.PyPIPackage = repoName("pypi", "hosted", 1), "pkg-0001"
	s.info.NPMRepo, s.info.NPMPackage = repoName("npm", "hosted", 1), "pkg-0001"
	s.info.OCITags, s.info.HeavyVersions = seedHeavyTags, seedHeavyVersions
	s.info.HugeTagImage, s.info.HugeTags = "pkg-0002", seedHugeTags
	s.info.ManyImagesRepo, s.info.ManyImages, s.info.ManyImagesTags = repoName("oci", "hosted", 2), seedManyImages, seedManyImagesTags

	for _, p := range seedProtocols {
		for _, r := range hosted[p.name] {
			for j := 1; j <= seedHostedPackages; j++ {
				s.genPackage(r, j)
			}
		}
		for _, r := range proxies[p.name] {
			for j := 1; j <= seedProxyPackages; j++ {
				s.genPackage(r, j)
			}
		}
	}
	s.info.Packages = len(s.packages)
	s.info.Blobs = len(s.blobs)
	s.info.Subjects = len(s.subjects)
	s.info.Evidence = len(s.evidence)
	s.info.Events = len(s.events)
}

func (s *seeder) blob(digest string, size int64) {
	if s.blobSeen[digest] {
		return
	}
	s.blobSeen[digest] = true
	s.blobs = append(s.blobs, []any{digest, size, "s3"})
}

func blobSize(digest string) int64 {
	var n int64
	for _, c := range digest[7:15] {
		n = n*16 + int64(c%16)
	}
	return 1024 + n%(200<<20)
}

// genPackage writes one package of r with its artifacts. Package 1 of the
// first hosted repository of each protocol is the known, heavy one.
func (s *seeder) genPackage(r seedRepo, j int) {
	name := fmt.Sprintf("pkg-%04d", j)
	pkgID := seedID("pkg", r.name, name)
	s.packages = append(s.packages, []any{pkgID, r.id, name, name})
	known := r.typ == "hosted" && r.index == 1 && j == 1

	n := 1 + j%3
	if known {
		n = seedHeavyVersions
		if r.protocol == "oci" {
			n = seedHeavyTags
		}
	}
	if r.protocol == "oci" && r.typ == "hosted" && r.index == 1 && j == 2 {
		n = seedHugeTags
	}
	if r.protocol == "oci" && r.typ == "hosted" && r.index == 2 && j <= seedManyImages {
		n = seedManyImagesTags
	}
	for k := 0; k < n; k++ {
		s.artifact++
		age := time.Duration(s.artifact%2000) * time.Hour
		switch r.protocol {
		case "oci":
			tag := "latest"
			if k > 0 {
				tag = fmt.Sprintf("v1.%d", k)
			}
			d := seedDigest("oci", r.name, name, tag)
			s.blob(d, 2000+blobSize(d)%4096)
			s.tags = append(s.tags, []any{r.id, pkgID, tag, d})
			if r.typ == "hosted" {
				s.repoBlobs = append(s.repoBlobs, []any{r.id, d})
				s.genSubject(r, d, "oci-manifest", name, known && k == 0, k, age)
				if known && k == 0 {
					s.info.OCIDigest = d
				}
			}
		default:
			ver := fmt.Sprintf("1.0.%d", k)
			ext := "tar.gz"
			media := "application/gzip"
			kind := "pypi-dist"
			if r.protocol == "npm" {
				ext, kind = "tgz", "npm-tarball"
			}
			verID := seedID("ver", r.name, name, ver)
			published := s.now.Add(-age)
			s.versions = append(s.versions, []any{verID, pkgID, ver, published})
			d := seedDigest(r.protocol, r.name, name, ver)
			s.blob(d, blobSize(d))
			s.files = append(s.files, []any{seedID("file", r.name, name, ver), verID, d, fmt.Sprintf("%s-%s.%s", name, ver, ext), media, []byte(`{}`)})
			if r.protocol == "npm" && k == n-1 {
				s.distTags = append(s.distTags, []any{pkgID, "latest", verID})
			}
			if r.typ == "hosted" {
				s.genSubject(r, d, kind, name, known && k == n-1, k, age)
				if known && k == n-1 {
					if r.protocol == "pypi" {
						s.info.PyPIDigest = d
					} else {
						s.info.NPMDigest = d
					}
				}
			}
		}
		s.info.Artifacts++
	}
}

// genSubject gives every seedSubjectEveryNth hosted artifact (and every known
// one) an identity, evidence, and lifecycle history.
func (s *seeder) genSubject(r seedRepo, digest, kind, pkg string, known bool, k int, age time.Duration) {
	n := s.artifact
	if !known && n%seedSubjectEveryNth != 0 {
		return
	}
	created := s.now.Add(-age)
	s.subjects = append(s.subjects, []any{digest, blobSize(digest), kind, created})
	s.owners = append(s.owners, []any{digest, "repository:" + r.name})
	s.provenance = append(s.provenance, []any{digest, "git", "https://git.example.com/" + r.name + "/" + pkg, fmt.Sprintf("build-%d", n)})

	// Evidence: an SBOM on every subject, a Sindri scan on most, a signature
	// on some. Its blobs exist as rows only.
	s.addEvidence(digest, "sbom", "application/spdx+json", `{}`, "ci", n, created.Add(time.Minute))
	switch {
	case known && kind == "oci-manifest":
		// Two platforms, one with many findings, and an older scan of the
		// first, so the page's "newest per platform" choice is exercised.
		knownPlatforms := []string{"linux/amd64", "linux/arm64"}
		s.addEvidence(digest, "scan", "application/json", scanMetadata("linux/amd64", knownPlatforms, 12, 2, 0, n-1), "sindri", n*10+1, created.Add(2*time.Minute))
		s.addEvidence(digest, "scan", "application/json", scanMetadata("linux/amd64", knownPlatforms, seedKnownScanFindings, 7, 5, n), "sindri", n*10+2, created.Add(3*time.Minute))
		s.addEvidence(digest, "scan", "application/json", scanMetadata("linux/arm64", knownPlatforms, 40, 3, 0, n), "sindri", n*10+3, created.Add(4*time.Minute))
	case known:
		s.addEvidence(digest, "scan", "application/json", scanMetadata("", nil, 25, 2, 0, n), "sindri", n*10+1, created.Add(2*time.Minute))
	case n%10 < 7:
		platform := ""
		var platforms []string
		if kind == "oci-manifest" {
			platform = "linux/amd64"
			platforms = []string{platform}
		}
		findings := 0
		if n%3 != 0 {
			findings = 1 + n%6
		}
		s.addEvidence(digest, "scan", "application/json", scanMetadata(platform, platforms, findings, 0, 0, n), "sindri", n*10+1, created.Add(2*time.Minute))
	}
	if known || n%3 == 0 {
		s.addEvidence(digest, "signature", "application/vnd.dev.cosign.simplesigning.v1+json", `{}`, "ci", n*10+4, created.Add(5*time.Minute))
	}

	// Lifecycle: approved up to some stage; the last stage of the known
	// subject is then revoked, and every seedRevokeEveryNth subject has its
	// first stage revoked, so both states appear across stages.
	reached := 1 + n%len(s.stages)
	if known {
		reached = len(s.stages)
	}
	at := created.Add(10 * time.Minute)
	for i := 0; i < reached; i++ {
		st := s.stages[i]
		s.events = append(s.events, []any{digest, r.id, st.id, "approve", "user:alice", "promoted", at.Add(time.Duration(i) * time.Hour)})
		revoked := (known && i == reached-1 && reached > 1) || (!known && i == 0 && n%seedRevokeEveryNth == 0)
		if revoked {
			s.events = append(s.events, []any{digest, r.id, st.id, "revoke", "user:bob", "CVE found", at.Add(time.Duration(i)*time.Hour + 30*time.Minute)})
		}
	}
}

func (s *seeder) addEvidence(subject, typ, media, metadata, by string, n int, at time.Time) {
	bd := seedDigest("evidence", subject, typ, fmt.Sprint(n))
	s.blob(bd, 512+blobSize(bd)%65536)
	s.evidence = append(s.evidence, []any{seedID("evidence", subject, typ, fmt.Sprint(n)), subject, typ, media, bd, []byte(metadata), at, by})
}

// scanMetadata is what the worker writes on a Sindri scan's evidence: the
// fields the UI reads, with findings listed as the worker lists them. A
// per-platform scan carries platforms, the sorted list of every platform of its
// job, as internal/scan writes it; without it the gate refuses the scan.
func scanMetadata(platform string, platforms []string, findings, critical, high, salt int) string {
	type vuln struct {
		ID        string `json:"id"`
		Severity  string `json:"severity"`
		Package   string `json:"package"`
		Installed string `json:"installed"`
		Fixed     string `json:"fixed"`
	}
	sevs := []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}
	counts := map[string]int{}
	vulns := make([]vuln, 0, findings)
	for i := 0; i < findings; i++ {
		sev := sevs[2+(i+salt)%3]
		switch {
		case i < critical:
			sev = "CRITICAL"
		case i < critical+high:
			sev = "HIGH"
		}
		counts[sev]++
		fixed := ""
		if i%3 != 0 {
			fixed = fmt.Sprintf("2.%d.%d", i%9, i%5+1)
		}
		vulns = append(vulns, vuln{
			ID: fmt.Sprintf("CVE-2026-%04d", 1000+(i*37+salt)%9000), Severity: sev,
			Package: fmt.Sprintf("libexample%d", i%40), Installed: fmt.Sprintf("1.%d.0", i%9), Fixed: fixed,
		})
	}
	meta := map[string]any{
		"producer": "sindri", "scanner": "trivy", "coverage": "image",
		"db_updated_at":   "2026-11-16T06:00:00Z",
		"severity_counts": counts, "vulnerabilities": vulns, "new_vulnerabilities": []string{},
	}
	if platform != "" {
		meta["platform"] = platform
		meta["platforms"] = platforms
	}
	b, err := json.Marshal(meta)
	if err != nil {
		panic(fmt.Sprintf("marshalling scan metadata: %v", err)) // impossible state: only strings and ints
	}
	return string(b)
}

// copyAll writes every table, parents before children.
func (s *seeder) copyAll(ctx context.Context, tx pgx.Tx) error {
	steps := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"lifecycle_stages", []string{"id", "name", "position"}, s.stageRows},
		{"blobs", []string{"digest", "size", "storage_backend"}, s.blobs},
		{"repositories", []string{"id", "name", "type", "protocol", "config", "locked", "managed_by"}, s.repos},
		{"remotes", []string{"repo_id", "upstream_url", "ttl_seconds", "negative_ttl_seconds"}, s.remotes},
		{"repository_members", []string{"virtual_repo_id", "member_repo_id", "position"}, s.members},
		{"packages", []string{"id", "repo_id", "normalized_name", "display_name"}, s.packages},
		{"versions", []string{"id", "package_id", "version", "published_at"}, s.versions},
		{"files", []string{"id", "version_id", "blob_digest", "filename", "media_type", "metadata"}, s.files},
		{"tags", []string{"repo_id", "package_id", "name", "target_digest"}, s.tags},
		{"dist_tags", []string{"package_id", "tag", "version_id"}, s.distTags},
		{"repository_blobs", []string{"repo_id", "digest"}, s.repoBlobs},
		{"subjects", []string{"digest", "size", "kind", "created_at"}, s.subjects},
		{"subject_owners", []string{"digest", "owner_ref"}, s.owners},
		{"provenance", []string{"digest", "source_type", "source_ref", "build_ref"}, s.provenance},
		{"evidence", []string{"id", "subject_digest", "type", "media_type", "blob_digest", "metadata", "created_at", "created_by"}, s.evidence},
		{"lifecycle_events", []string{"subject_digest", "repo_id", "stage_id", "action", "actor", "reason", "at"}, s.events},
	}
	for _, st := range steps {
		if len(st.rows) == 0 {
			continue
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{st.table}, st.cols, pgx.CopyFromRows(st.rows)); err != nil {
			return fmt.Errorf("seeding %s: %w", st.table, err)
		}
	}
	return nil
}
