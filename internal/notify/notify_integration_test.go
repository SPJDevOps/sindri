//go:build integration

package notify_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/internal/worker"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func digestOf(t *testing.T, c string) model.Digest {
	t.Helper()
	d, err := model.ParseDigest("sha256:" + strings.Repeat(c, 64))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func findingEvent(t *testing.T, key string) notify.Event {
	return notify.Event{
		Digest: digestOf(t, "a"), Repository: "docker", DedupeKey: key,
		Data: notify.FindingData{NewVulnerabilities: []string{"CVE-2026-1"}, SeverityCounts: map[string]int{"HIGH": 1}, EvidenceID: "7"},
	}
}

func hook(url string) notify.Webhook {
	return notify.Webhook{Name: "sec", URL: url, Secret: testSecret, Events: []string{notify.EventFindingNew}, Timeout: 2 * time.Second}
}

func emit(t *testing.T, n *notify.Notifier, database *db.DB, ev notify.Event) int {
	t.Helper()
	got, err := n.Emit(t.Context(), database.Pool(), ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return got
}

func TestEmit_MatchingWebhookGetsARowAndAJob(t *testing.T) {
	database := testsupport.MigratedDB(t)
	n := notify.New(notify.Options{
		Webhooks:    []notify.Webhook{hook("https://a.example/x"), {Name: "other", URL: "https://b.example", Events: []string{notify.EventPromoted}}},
		MaxAttempts: 3, PublicURL: "https://sindri.example", Version: "9.9.9",
	})
	if got := emit(t, n, database, findingEvent(t, "finding:7")); got != 1 {
		t.Fatalf("Emit created %d deliveries, want 1", got)
	}

	rows, err := database.Queries().ListWebhookDeliveries(t.Context(), db.ListWebhookDeliveriesParams{RowLimit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListWebhookDeliveries = %d rows, %v", len(rows), err)
	}
	row := rows[0]
	if row.Webhook != "sec" || row.State != "pending" || row.Event != "finding.new" || !row.JobID.Valid || !row.JobRunAfter.Valid {
		t.Errorf("delivery = %+v", row)
	}
	job, err := queue.New(database).Get(t.Context(), row.JobID.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != notify.JobKind || job.MaxAttempts != 3 {
		t.Errorf("job = %+v", job)
	}

	var env notify.Envelope
	if err := json.Unmarshal(row.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.ID != strconv.FormatInt(row.ID, 10) || env.Subject.Digest != digestOf(t, "a").String() || env.Subject.Stage != "" ||
		env.Sindri.URL != "https://sindri.example" || env.Sindri.Version != "9.9.9" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestEmit_RepeatedDedupeKeyIsSkippedAndUnmatchedCostsNothing(t *testing.T) {
	database := testsupport.MigratedDB(t)
	h := hook("https://a.example/x")
	h.Repositories = []string{"pypi"}
	n := notify.New(notify.Options{Webhooks: []notify.Webhook{h, {Name: "all", URL: "https://b.example/x", Events: []string{notify.EventFindingNew}}}})

	if got := emit(t, n, database, findingEvent(t, "finding:7")); got != 1 {
		t.Fatalf("first Emit created %d, want 1 (the repository filter excludes sec)", got)
	}
	if got := emit(t, n, database, findingEvent(t, "finding:7")); got != 0 {
		t.Errorf("a repeated dedupe key created %d deliveries", got)
	}
	promo := notify.Event{Digest: digestOf(t, "a"), Repository: "docker", DedupeKey: "promoted:1", Data: notify.PromotedData{Actor: "a"}}
	if got := emit(t, n, database, promo); got != 0 {
		t.Errorf("an event nobody subscribes to created %d deliveries", got)
	}
}

func TestEmitTx_RolledBackTransactionLeavesNothing(t *testing.T) {
	database := testsupport.MigratedDB(t)
	n := notify.New(notify.Options{Webhooks: []notify.Webhook{hook("https://a.example/x")}})
	tx, err := database.Pool().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.EmitTx(t.Context(), db.New(tx), findingEvent(t, "finding:1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := database.Queries().ListWebhookDeliveries(t.Context(), db.ListWebhookDeliveriesParams{RowLimit: 10})
	if len(rows) != 0 {
		t.Errorf("%d deliveries survived a rollback", len(rows))
	}
}

// receiver records requests and answers with a scripted status.
type receiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	reqs   []*http.Request
	bodies [][]byte
	delay  time.Duration
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	r := &receiver{status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, req)
		r.bodies = append(r.bodies, body)
		status, delay := r.status, r.delay
		r.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-req.Context().Done():
			}
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(strings.Repeat("x", 10_000)))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// delivery emits one finding event and returns the job for its row.
func setup(t *testing.T, h notify.Webhook) (*notify.Notifier, *notify.Deliverer, *db.DB, queue.Job, int64) {
	t.Helper()
	database := testsupport.MigratedDB(t)
	n := notify.New(notify.Options{Webhooks: []notify.Webhook{h}, MaxAttempts: 3})
	d := notify.NewDeliverer(notify.DelivererOptions{
		Notifier: n, DB: database, Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	emit(t, n, database, findingEvent(t, "finding:7"))
	rows, err := database.Queries().ListWebhookDeliveries(t.Context(), db.ListWebhookDeliveriesParams{RowLimit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("no delivery: %v", err)
	}
	job := queue.Job{ID: rows[0].JobID.Int64, Kind: notify.JobKind, Args: json.RawMessage(`{"delivery":` + strconv.FormatInt(rows[0].ID, 10) + `}`), Attempts: 1, MaxAttempts: 3}
	return n, d, database, job, rows[0].ID
}

func TestHandle_SignedPostIsDeliveredOnTwoHundred(t *testing.T) {
	rcv := newReceiver(t, http.StatusNoContent)
	_, d, database, job, id := setup(t, hook(rcv.srv.URL+"/hook/tok3n?k=tok3n"))

	result, err := d.Handle(t.Context(), job)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(result) != `{"status":204}` {
		t.Errorf("result = %s", result)
	}
	if rcv.count() != 1 {
		t.Fatalf("receiver got %d requests", rcv.count())
	}
	req, body := rcv.reqs[0], rcv.bodies[0]
	if req.Header.Get("X-Sindri-Event") != "finding.new" || req.Header.Get("X-Sindri-Delivery") != strconv.FormatInt(id, 10) ||
		req.Header.Get("X-Sindri-Timestamp") != "1700000000" || req.Header.Get("Content-Type") != "application/json" ||
		!strings.HasPrefix(req.Header.Get("User-Agent"), "sindri/") {
		t.Errorf("headers = %v", req.Header)
	}
	if want := notify.Sign(testSecret, 1_700_000_000, body); req.Header.Get("X-Sindri-Signature") != want {
		t.Errorf("signature = %q, want %q", req.Header.Get("X-Sindri-Signature"), want)
	}
	var env notify.Envelope
	if err := json.Unmarshal(body, &env); err != nil || env.ID != strconv.FormatInt(id, 10) {
		t.Errorf("body = %s (%v)", body, err)
	}

	row, err := database.Queries().GetWebhookDelivery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "delivered" || row.Attempts != 1 || row.LastStatus.Int32 != 204 || !row.DeliveredAt.Valid {
		t.Errorf("row = %+v", row)
	}
}

func TestHandle_FailureModes(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		delay         time.Duration
		attempt       int
		wantPermanent bool
		wantState     string
		wantStatus    int32
	}{
		{name: "ServerErrorRetriesWhileAttemptsRemain", status: 500, attempt: 1, wantState: "pending", wantStatus: 500},
		{name: "ServerErrorOnTheLastAttemptFailsTheDelivery", status: 503, attempt: 3, wantState: "failed", wantStatus: 503},
		{name: "TooManyRequestsRetries", status: 429, attempt: 1, wantState: "pending", wantStatus: 429},
		{name: "NotFoundFailsAtOnce", status: 404, attempt: 1, wantPermanent: true, wantState: "failed", wantStatus: 404},
		{name: "RedirectIsNotFollowedAndFailsAtOnce", status: 302, attempt: 1, wantPermanent: true, wantState: "failed", wantStatus: 302},
		{name: "SlowReceiverTimesOutAndRetries", status: 200, delay: 3 * time.Second, attempt: 1, wantState: "pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newReceiver(t, tc.status)
			rcv.delay = tc.delay
			h := hook(rcv.srv.URL + "/hook/tok3n?k=tok3n")
			h.Timeout = 500 * time.Millisecond
			_, d, database, job, id := setup(t, h)
			job.Attempts = tc.attempt

			_, err := d.Handle(t.Context(), job)
			if err == nil {
				t.Fatal("Handle succeeded")
			}
			if errors.Is(err, worker.ErrPermanent) != tc.wantPermanent {
				t.Errorf("permanent = %t, want %t (%v)", !tc.wantPermanent, tc.wantPermanent, err)
			}
			if strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), "sec") {
				t.Errorf("error %q must name the webhook and never the path or query", err)
			}
			row, err := database.Queries().GetWebhookDelivery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != tc.wantState || row.Attempts != 1 || row.LastStatus.Int32 != tc.wantStatus || !row.LastError.Valid ||
				strings.Contains(row.LastError.String, "tok3n") {
				t.Errorf("row = %+v", row)
			}
			if tc.status == http.StatusFound && rcv.count() != 1 {
				t.Errorf("a redirect was followed: %d requests", rcv.count())
			}
		})
	}
}

func TestHandle_UnreachableReceiverRetriesWithoutLeakingTheURL(t *testing.T) {
	rcv := newReceiver(t, 200)
	url := rcv.srv.URL + "/hook/tok3n"
	rcv.srv.Close()
	_, d, database, job, id := setup(t, hook(url))

	_, err := d.Handle(t.Context(), job)
	if err == nil || errors.Is(err, worker.ErrPermanent) || strings.Contains(err.Error(), "tok3n") {
		t.Fatalf("err = %v, want a retryable error without the path", err)
	}
	row, _ := database.Queries().GetWebhookDelivery(t.Context(), id)
	if row.State != "pending" || row.Attempts != 1 || row.LastStatus.Valid || strings.Contains(row.LastError.String, "tok3n") {
		t.Errorf("row = %+v", row)
	}
}

func TestHandle_WebhookRemovedFromConfigurationFailsPermanently(t *testing.T) {
	rcv := newReceiver(t, 200)
	n, d, database, job, id := setup(t, hook(rcv.srv.URL))
	n.Swap(nil)

	_, err := d.Handle(t.Context(), job)
	if !errors.Is(err, worker.ErrPermanent) || !strings.Contains(err.Error(), "no longer configured") {
		t.Fatalf("err = %v", err)
	}
	row, _ := database.Queries().GetWebhookDelivery(t.Context(), id)
	if row.State != "failed" || rcv.count() != 0 {
		t.Errorf("row = %+v, receiver saw %d requests", row, rcv.count())
	}
}

func TestHandle_MissingDeliveryFailsPermanently(t *testing.T) {
	_, d, _, job, _ := setup(t, hook("https://a.example"))
	job.Args = json.RawMessage(`{"delivery":999999}`)
	if _, err := d.Handle(t.Context(), job); !errors.Is(err, worker.ErrPermanent) {
		t.Errorf("err = %v, want permanent", err)
	}
}

func TestRedeliver_CopiesThePayloadIntoANewDelivery(t *testing.T) {
	rcv := newReceiver(t, 200)
	n, d, database, job, id := setup(t, hook(rcv.srv.URL))
	if _, err := d.Handle(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	fresh, err := n.Redeliver(t.Context(), database.Pool(), id)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if fresh.ID == id || fresh.State != "pending" || !fresh.JobID.Valid {
		t.Errorf("redelivery = %+v", fresh)
	}
	var env notify.Envelope
	if err := json.Unmarshal(fresh.Payload, &env); err != nil || env.ID != strconv.FormatInt(fresh.ID, 10) || env.Event != "finding.new" {
		t.Errorf("payload = %s (%v)", fresh.Payload, err)
	}
	old, _ := database.Queries().GetWebhookDelivery(t.Context(), id)
	if old.State != "delivered" {
		t.Errorf("the original changed: %+v", old)
	}
	if _, err := n.Redeliver(t.Context(), database.Pool(), 999999); !errors.Is(err, notify.ErrDeliveryNotFound) {
		t.Errorf("Redeliver(unknown) = %v", err)
	}
	n.Swap(nil)
	if _, err := n.Redeliver(t.Context(), database.Pool(), id); !errors.Is(err, notify.ErrUnknownWebhook) {
		t.Errorf("Redeliver(removed webhook) = %v", err)
	}
}

// failAttemptWrites makes every update that bumps a delivery's attempt count
// fail, so RecordWebhookAttempt returns a plain database error while
// MarkWebhookFailed still works.
func failAttemptWrites(t *testing.T, database *db.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_attempts() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'attempt writes refused'; END $$`,
		`CREATE TRIGGER refuse_attempts BEFORE UPDATE ON webhook_deliveries FOR EACH ROW WHEN (NEW.attempts <> OLD.attempts) EXECUTE FUNCTION refuse_attempts()`,
	} {
		if _, err := database.Pool().Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHandle_DatabaseErrorOnTheLastAttemptStillFailsTheDelivery(t *testing.T) {
	tests := []struct {
		name      string
		attempt   int
		wantState string
	}{
		{name: "LastAttemptMarksTheRowFailed", attempt: 3, wantState: "failed"},
		{name: "EarlierAttemptLeavesItPending", attempt: 1, wantState: "pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newReceiver(t, 500)
			_, d, database, job, id := setup(t, hook(rcv.srv.URL))
			failAttemptWrites(t, database)
			job.Attempts = tc.attempt

			if _, err := d.Handle(t.Context(), job); err == nil {
				t.Fatal("Handle succeeded")
			}
			row, err := database.Queries().GetWebhookDelivery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != tc.wantState {
				t.Errorf("state = %q, want %q", row.State, tc.wantState)
			}
		})
	}
}

func TestSweepOrphaned_FailsPendingDeliveriesWhoseJobEndedOrIsGone(t *testing.T) {
	tests := []struct {
		name      string
		prepare   string // SQL run with $1 = the job id and $2 = the delivery id
		wantState string
	}{
		{name: "FailedJobOrphansTheDelivery", prepare: `UPDATE jobs SET state = 'failed', finished_at = now() WHERE id = $1`, wantState: "failed"},
		{name: "DeletedJobOrphansTheDelivery", prepare: `DELETE FROM jobs WHERE id = $1`, wantState: "failed"},
		{name: "QueuedJobKeepsTheDeliveryPending", prepare: `SELECT $1::bigint`, wantState: "pending"},
		{name: "RunningJobKeepsTheDeliveryPending", prepare: `UPDATE jobs SET state = 'running' WHERE id = $1`, wantState: "pending"},
		{name: "DeliveryNewerThanAMinuteIsLeftAlone", prepare: `UPDATE jobs SET state = 'failed' WHERE id = $1; UPDATE webhook_deliveries SET created_at = now() WHERE id = $2`, wantState: "pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, database, job, id := setup(t, hook("https://a.example"))
			ctx := t.Context()
			if _, err := database.Pool().Exec(ctx, `UPDATE webhook_deliveries SET created_at = now() - interval '2 minutes' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range strings.Split(tc.prepare, ";") {
				args := []any{job.ID}
				if !strings.Contains(stmt, "$1") {
					args = []any{id}
					stmt = strings.ReplaceAll(stmt, "$2", "$1")
				}
				if _, err := database.Pool().Exec(ctx, stmt, args...); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}

			swept, err := notify.SweepOrphaned(ctx, database.Queries())
			if err != nil {
				t.Fatalf("SweepOrphaned: %v", err)
			}
			row, err := database.Queries().GetWebhookDelivery(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != tc.wantState || (tc.wantState == "failed") != (swept == 1) {
				t.Errorf("state = %q after sweeping %d, want %q", row.State, swept, tc.wantState)
			}
			if tc.wantState == "failed" && row.LastError.String != "the delivery job ended without an outcome" {
				t.Errorf("last_error = %q", row.LastError.String)
			}
		})
	}
}

func TestSweepOrphaned_LeavesFinishedDeliveriesAlone(t *testing.T) {
	rcv := newReceiver(t, 200)
	_, d, database, job, id := setup(t, hook(rcv.srv.URL))
	if _, err := d.Handle(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool().Exec(t.Context(), `UPDATE webhook_deliveries SET created_at = now() - interval '2 minutes'; DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	swept, err := notify.SweepOrphaned(t.Context(), database.Queries())
	row, _ := database.Queries().GetWebhookDelivery(t.Context(), id)
	if err != nil || swept != 0 || row.State != "delivered" {
		t.Errorf("swept %d (%v), state %q", swept, err, row.State)
	}
}
