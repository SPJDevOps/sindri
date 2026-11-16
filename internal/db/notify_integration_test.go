//go:build integration

package db_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/testsupport"
)

func TestSubjectHasLiveApproval_FollowsTheLatestEventPerStage(t *testing.T) {
	database := testsupport.MigratedDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool().Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	digest := fmt.Sprintf("sha256:%064x", 1)
	exec(`INSERT INTO blobs (digest, size, storage_backend) VALUES ($1, 1, 'memory')`, digest)
	exec(`INSERT INTO subjects (digest, size, kind) VALUES ($1, 1, 'oci-manifest')`, digest)
	exec(`INSERT INTO repositories (name, type, protocol, managed_by) VALUES ('docker', 'hosted', 'oci', 'api')`)
	exec(`INSERT INTO lifecycle_stages (name, position) VALUES ('dev', 0), ('prod', 1)`)
	event := func(stage, action string) {
		exec(`INSERT INTO lifecycle_events (subject_digest, repo_id, stage_id, action, actor)
		      SELECT $1, r.id, s.id, $3, 'tester' FROM repositories r, lifecycle_stages s WHERE r.name = 'docker' AND s.name = $2`,
			digest, stage, action)
	}
	q := database.Queries()
	approved := func() bool {
		t.Helper()
		got, err := q.SubjectHasLiveApproval(t.Context(), digest)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	stages := func() []string {
		t.Helper()
		rows, err := q.ListApprovedStages(t.Context(), digest)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}

	if approved() || len(stages()) != 0 {
		t.Fatal("a subject with no events is approved")
	}
	event("dev", "approve")
	event("prod", "approve")
	if !approved() || fmt.Sprint(stages()) != "[dev prod]" {
		t.Errorf("approved = %t, stages = %v, want true, [dev prod]", approved(), stages())
	}
	event("prod", "revoke")
	if !approved() || fmt.Sprint(stages()) != "[dev]" {
		t.Errorf("after revoking prod: approved = %t, stages = %v, want true, [dev]", approved(), stages())
	}
	event("dev", "revoke")
	if approved() || len(stages()) != 0 {
		t.Errorf("after revoking every stage: approved = %t, stages = %v", approved(), stages())
	}
}

func TestListWebhookDeliveries_FiltersAndOrdersNewestFirst(t *testing.T) {
	database := testsupport.MigratedDB(t)
	q := database.Queries()
	insert := func(webhook, digest, key string) db.WebhookDelivery {
		t.Helper()
		row, err := q.InsertWebhookDelivery(t.Context(), db.InsertWebhookDeliveryParams{
			Webhook: webhook, Event: "finding.new", SubjectDigest: digest, Repository: "docker", Payload: []byte(`{}`), DedupeKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	a := insert("a", "sha256:1", "k1")
	b := insert("b", "sha256:2", "k1")
	c := insert("a", "sha256:2", "k2")
	if err := q.MarkWebhookFailed(t.Context(), db.MarkWebhookFailedParams{ID: c.ID, LastError: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordWebhookAttempt(t.Context(), db.RecordWebhookAttemptParams{ID: c.ID, LastStatus: pgtype.Int4{Int32: 500, Valid: true}}); err != nil {
		t.Fatal(err)
	}

	list := func(p db.ListWebhookDeliveriesParams) []int64 {
		t.Helper()
		p.RowLimit = 10
		rows, err := q.ListWebhookDeliveries(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return ids
	}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	for name, tc := range map[string]struct {
		params db.ListWebhookDeliveriesParams
		want   []int64
	}{
		"NoFilterNewestFirst": {db.ListWebhookDeliveriesParams{}, []int64{c.ID, b.ID, a.ID}},
		"ByWebhook":           {db.ListWebhookDeliveriesParams{Webhook: text("a")}, []int64{c.ID, a.ID}},
		"ByState":             {db.ListWebhookDeliveriesParams{State: text("failed")}, []int64{c.ID}},
		"ByDigest":            {db.ListWebhookDeliveriesParams{SubjectDigest: text("sha256:2")}, []int64{c.ID, b.ID}},
	} {
		if got := list(tc.params); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}

	counts, err := q.CountFailedWebhookDeliveriesByWebhook(t.Context())
	if err != nil || len(counts) != 1 || counts[0].Webhook != "a" || counts[0].Failed != 1 {
		t.Errorf("failed counts = %+v, %v", counts, err)
	}
}
