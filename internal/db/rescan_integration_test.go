//go:build integration

package db_test

import (
	"fmt"
	"testing"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/testsupport"
)

// rescanFixture seeds subjects directly with SQL, so the queries are tested
// against the schema and not against the services that normally write it.
type rescanFixture struct {
	t    *testing.T
	exec func(sql string, args ...any)
	n    int
}

func (f *rescanFixture) digest() string {
	f.n++
	return fmt.Sprintf("sha256:%064x", f.n)
}

func (f *rescanFixture) repo(name, typ string) {
	f.exec(`INSERT INTO repositories (name, type, protocol, managed_by) VALUES ($1, $2, 'oci', 'api')`, name, typ)
}

// subject creates a subject owned by owners; blobs back the subject itself.
func (f *rescanFixture) subject(kind string, owners ...string) string {
	d := f.digest()
	f.exec(`INSERT INTO blobs (digest, size, storage_backend) VALUES ($1, 1, 'memory')`, d)
	f.exec(`INSERT INTO subjects (digest, size, kind) VALUES ($1, 1, $2)`, d, kind)
	for _, o := range owners {
		f.exec(`INSERT INTO subject_owners (digest, owner_ref) VALUES ($1, $2)`, d, "repository:"+o)
	}
	return d
}

func (f *rescanFixture) event(digest, repo, action string) {
	f.exec(`INSERT INTO lifecycle_events (subject_digest, repo_id, stage_id, action, actor)
	        SELECT $1, r.id, s.id, $3, 'test' FROM repositories r, lifecycle_stages s WHERE r.name = $2 AND s.name = 'prod'`,
		digest, repo, action)
}

// scan attaches scan evidence; producer "" leaves the metadata empty, as a
// client-attached document does.
func (f *rescanFixture) scan(digest, producer, counts string) {
	meta := `{}`
	if producer != "" {
		meta = fmt.Sprintf(`{"producer":%q,"platform":"linux/amd64","severity_counts":%s}`, producer, counts)
	}
	blob := f.digest()
	f.exec(`INSERT INTO blobs (digest, size, storage_backend) VALUES ($1, 1, 'memory')`, blob)
	f.exec(`INSERT INTO evidence (subject_digest, type, media_type, blob_digest, metadata, created_by)
	        VALUES ($1, 'scan', 'application/json', $2, $3::jsonb, 'test')`, digest, blob, meta)
}

func newRescanFixture(t *testing.T) (*db.DB, *rescanFixture) {
	t.Helper()
	database := testsupport.MigratedDB(t)
	f := &rescanFixture{t: t}
	f.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool().Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	f.exec(`INSERT INTO lifecycle_stages (name, position) VALUES ('prod', 0)`)
	return database, f
}

func TestListApprovedSubjectsForRescan_PicksLowestHostedOwnerOfApprovedSubjects(t *testing.T) {
	database, f := newRescanFixture(t)
	f.repo("zeta", "hosted")
	f.repo("alpha", "hosted")
	f.repo("a-proxy", "proxy")

	multi := f.subject("oci-manifest", "zeta", "alpha", "a-proxy")
	f.event(multi, "zeta", "approve")
	revoked := f.subject("oci-manifest", "zeta")
	f.event(revoked, "zeta", "approve")
	f.event(revoked, "zeta", "revoke")
	f.subject("pypi-dist", "alpha")
	proxyOnly := f.subject("oci-manifest", "a-proxy")
	f.event(proxyOnly, "a-proxy", "approve")

	rows, err := database.Queries().ListApprovedSubjectsForRescan(t.Context())
	if err != nil {
		t.Fatalf("ListApprovedSubjectsForRescan: %v", err)
	}
	if len(rows) != 1 || rows[0].Digest != multi || rows[0].Repository != "alpha" || rows[0].Kind != "oci-manifest" {
		t.Fatalf("rows = %+v, want only %s in alpha", rows, multi)
	}
}

func TestCountApprovedSubjectsBySeverity(t *testing.T) {
	database, f := newRescanFixture(t)
	f.repo("r", "hosted")

	critical := f.subject("oci-manifest", "r")
	f.event(critical, "r", "approve")
	f.scan(critical, "sindri", `{"CRITICAL":2,"HIGH":0,"LOW":1}`)

	superseded := f.subject("oci-manifest", "r")
	f.event(superseded, "r", "approve")
	f.scan(superseded, "sindri", `{"CRITICAL":1}`)
	f.scan(superseded, "sindri", `{"CRITICAL":0,"MEDIUM":3}`)

	revoked := f.subject("oci-manifest", "r")
	f.event(revoked, "r", "approve")
	f.event(revoked, "r", "revoke")
	f.scan(revoked, "sindri", `{"CRITICAL":5}`)

	attached := f.subject("oci-manifest", "r")
	f.event(attached, "r", "approve")
	f.scan(attached, "", "")

	rows, err := database.Queries().CountApprovedSubjectsBySeverity(t.Context())
	if err != nil {
		t.Fatalf("CountApprovedSubjectsBySeverity: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Severity] = r.Subjects
	}
	want := map[string]int64{"CRITICAL": 1, "MEDIUM": 1, "LOW": 1}
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("counts = %v, want %v", got, want)
			break
		}
	}
}
