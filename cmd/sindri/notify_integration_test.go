//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spj/sindri/internal/notify"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const (
	hookSecret = "0123456789abcdef0123456789abcdef-test-secret"
	// hookPath stands for a token in a real receiver URL; it must never be
	// shown by the API.
	hookPath = "/hooks/t0ken-must-not-leak"
)

// received is one delivery the receiver saw.
type received struct {
	header http.Header
	body   []byte
	// signed is whether X-Sindri-Signature matched the body and timestamp.
	signed   bool
	envelope notify.Envelope
}

// receiver is an httptest webhook endpoint that verifies signatures.
type receiver struct {
	srv    *httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	got    []received
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	rc := &receiver{}
	rc.status.Store(int32(status)) //nolint:gosec // an HTTP status
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get("X-Sindri-Timestamp"), 10, 64)
		in := received{header: r.Header.Clone(), body: body}
		in.signed = r.URL.Path == hookPath && r.Header.Get("X-Sindri-Signature") == notify.Sign([]byte(hookSecret), ts, body)
		_ = json.Unmarshal(body, &in.envelope)
		rc.mu.Lock()
		rc.got = append(rc.got, in)
		rc.mu.Unlock()
		w.WriteHeader(int(rc.status.Load()))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) all() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]received(nil), rc.got...)
}

// notifyStack runs serve with scanning, one hosted OCI repository, a dev
// stage, and one webhook pointing at rc.
func notifyStack(t *testing.T, dbURL, stub string, rc *receiver, maxAttempts int, blobs *memstore.Store) *stack {
	t.Helper()
	return notifyStackRepos(t, dbURL, stub, rc, maxAttempts, blobs, []string{"docker"}, "")
}

// notifyStackRepos is notifyStack with several hosted OCI repositories and an
// optional repositories filter on the webhook (a YAML flow list, or empty for
// none).
func notifyStackRepos(t *testing.T, dbURL, stub string, rc *receiver, maxAttempts int, blobs *memstore.Store, repos []string, filter string) *stack {
	t.Helper()
	var repoYAML strings.Builder
	for _, r := range repos {
		fmt.Fprintf(&repoYAML, "  %s: { protocol: oci }\n", r)
	}
	if filter != "" {
		filter = "      repositories: " + filter + "\n"
	}
	body := fmt.Sprintf("scanning:\n  enabled: true\n  trivy: %q\n  cacheDir: %q\n  rescanInterval: 0s\n"+
		"notifications:\n  maxAttempts: %d\n  webhooks:\n    - name: security\n      url: %s%s\n      secret: %s\n"+
		"      events: [finding.new, lifecycle.promoted]\n      timeout: 5s\n%s"+
		"repos:\n%slifecycle:\n  stages: [dev]\n",
		stub, t.TempDir(), maxAttempts, rc.srv.URL, hookPath, hookSecret, filter, repoYAML.String())
	return startServeOn(t, "127.0.0.1:0", dbURL, "", body, func(h *serveHooks) {
		scanHooks(h)
		h.build.Blobs = blobs
	})
}

type deliveryItem struct {
	ID          int64           `json:"id"`
	Webhook     string          `json:"webhook"`
	Event       string          `json:"event"`
	Digest      string          `json:"digest"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	LastStatus  *int            `json:"last_status"`
	LastError   *string         `json:"last_error"`
	NextRetryAt *string         `json:"next_retry_at"`
	Payload     json.RawMessage `json:"payload"`
}

func (s *stack) deliveries(query string) []deliveryItem {
	s.t.Helper()
	r := s.request(http.MethodGet, "/api/v1/notifications/deliveries"+query, nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		s.t.Fatalf("list deliveries: %d %s", r.status, r.body)
	}
	var out struct {
		Items []deliveryItem `json:"items"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		s.t.Fatalf("deliveries: %v\n%s", err, r.body)
	}
	return out.Items
}

func (s *stack) metric(line string) bool {
	return strings.Contains(string(s.request(http.MethodGet, "/metrics", nil, nil).body), line)
}

// awaitScansFinished waits until every scan job is done, so a test that
// asserts nothing was sent has seen the job's last act.
func awaitScansFinished(t *testing.T, dbURL string, n int) {
	t.Helper()
	eventually(t, "the scan jobs to finish", func() bool {
		jobs := scanJobs(t, dbURL)
		if len(jobs) < n {
			return false
		}
		for _, j := range jobs {
			if j.State != "succeeded" {
				return false
			}
		}
		return true
	})
}

func TestNotifications_PromotionDeliversOneSignedWebhook(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, _ := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStack(t, db, stub, rc, 3, memstore.New())

	digest := s.push("docker/demo/app", "1.0", "notify-promote")
	s.promoteTo(digest, "dev")

	eventually(t, "the webhook", func() bool { return len(rc.all()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	got := rc.all()
	if len(got) != 1 {
		t.Fatalf("receiver saw %d deliveries, want 1", len(got))
	}
	in := got[0]
	if !in.signed {
		t.Errorf("signature did not verify: %q", in.header.Get("X-Sindri-Signature"))
	}
	if in.header.Get("X-Sindri-Event") != "lifecycle.promoted" || in.envelope.Event != "lifecycle.promoted" {
		t.Errorf("event = %q / %q", in.header.Get("X-Sindri-Event"), in.envelope.Event)
	}
	if in.envelope.Subject.Digest != digest || in.envelope.Subject.Repository != "docker" || in.envelope.Subject.Stage != "dev" {
		t.Errorf("subject = %+v, want %s in docker at dev", in.envelope.Subject, digest)
	}
	var data notify.PromotedData
	if err := json.Unmarshal(in.envelope.Data, &data); err != nil || data.Reason != "rescan test" || data.EventID == 0 {
		t.Errorf("data = %+v (%v)", data, err)
	}
	if in.header.Get("X-Sindri-Delivery") != in.envelope.ID {
		t.Errorf("delivery header %q != envelope id %q", in.header.Get("X-Sindri-Delivery"), in.envelope.ID)
	}

	eventually(t, "the delivery to be recorded", func() bool {
		d := s.deliveries("?webhook=security&state=delivered")
		return len(d) == 1 && d[0].Attempts == 1
	})
	eventually(t, "the delivered metric", func() bool { return s.metric(`sindri_webhook_deliveries_total{result="delivered"} 1`) })
}

func TestNotifications_RescanNewFindingOnApprovedSubjectDeliversOnce(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStack(t, db, stub, rc, 3, memstore.New())

	digest := s.push("docker/demo/app", "1.0", "notify-finding")
	eventually(t, "the first scan", func() bool { return len(s.scans(digest)) == 1 })
	awaitScansFinished(t, db, 1)
	s.promoteTo(digest, "dev")
	eventually(t, "the promotion webhook", func() bool { return len(rc.all()) == 1 })

	newerDatabase()
	if r := s.request(http.MethodPost, "/api/v1/subjects/"+digest+"/scan", nil, bearer(bootstrapToken)); r.status != http.StatusAccepted {
		t.Fatalf("POST scan: %d %s", r.status, r.body)
	}
	eventually(t, "the finding webhook", func() bool { return len(rc.all()) == 2 })
	awaitScansFinished(t, db, 2)
	time.Sleep(300 * time.Millisecond)

	got := rc.all()
	if len(got) != 2 {
		t.Fatalf("receiver saw %d deliveries, want the promotion and one finding", len(got))
	}
	in := got[1]
	if in.envelope.Event != "finding.new" || !in.signed {
		t.Fatalf("second delivery = %s signed=%t, want a signed finding.new", in.envelope.Event, in.signed)
	}
	var data notify.FindingData
	if err := json.Unmarshal(in.envelope.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.NewVulnerabilities) != 1 || data.NewVulnerabilities[0] != rescanNewCVE {
		t.Errorf("newVulnerabilities = %v, want [%s]", data.NewVulnerabilities, rescanNewCVE)
	}
	if len(data.ApprovedStages) != 1 || data.ApprovedStages[0] != "dev" || data.EvidenceID == "" {
		t.Errorf("data = %+v, want approved stage dev and an evidence id", data)
	}
	if in.envelope.Subject.Digest != digest || in.envelope.Subject.Stage != "" {
		t.Errorf("subject = %+v", in.envelope.Subject)
	}
}

func TestNotifications_UnapprovedSubjectFindingDoesNotNotify(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStack(t, db, stub, rc, 3, memstore.New())

	digest := s.push("docker/demo/app", "1.0", "notify-unapproved")
	eventually(t, "the first scan", func() bool { return len(s.scans(digest)) == 1 })
	awaitScansFinished(t, db, 1)

	newerDatabase()
	if r := s.request(http.MethodPost, "/api/v1/subjects/"+digest+"/scan", nil, bearer(bootstrapToken)); r.status != http.StatusAccepted {
		t.Fatalf("POST scan: %d %s", r.status, r.body)
	}
	if got := s.awaitNewFindings(digest); len(got) != 1 {
		t.Fatalf("new_vulnerabilities = %v, want one", got)
	}
	awaitScansFinished(t, db, 2)
	time.Sleep(300 * time.Millisecond)
	if got := rc.all(); len(got) != 0 {
		t.Fatalf("receiver saw %d deliveries for an unapproved subject, want 0", len(got))
	}
	if d := s.deliveries(""); len(d) != 0 {
		t.Fatalf("delivery log = %+v, want empty", d)
	}
}

func TestNotifications_ReceiverDownRetriesThenRecordsFailure(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, _ := rescanStub(t)
	rc := newReceiver(t, http.StatusServiceUnavailable)
	s := notifyStack(t, db, stub, rc, 3, memstore.New())

	digest := s.push("docker/demo/app", "1.0", "notify-down")
	s.promoteTo(digest, "dev")

	eventually(t, "the delivery to fail", func() bool {
		d := s.deliveries("?state=failed")
		return len(d) == 1
	})
	d := s.deliveries("?state=failed")[0]
	if d.Attempts != 3 || d.LastStatus == nil || *d.LastStatus != 503 || d.LastError == nil || d.NextRetryAt != nil {
		t.Errorf("delivery = %+v, want 3 attempts, status 503, an error, and no next retry", d)
	}
	if n := len(rc.all()); n != 3 {
		t.Errorf("receiver saw %d attempts, want 3", n)
	}
	eventually(t, "the metrics", func() bool {
		return s.metric(`sindri_webhook_deliveries_total{result="failed"} 1`) && s.metric(`sindri_webhook_deliveries_total{result="retried"} 2`)
	})

	r := s.request(http.MethodGet, "/api/v1/notifications/webhooks", nil, bearer(bootstrapToken))
	var list struct {
		Items []struct {
			Name             string `json:"name"`
			Target           string `json:"target"`
			FailedDeliveries int    `json:"failed_deliveries"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.body, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("webhooks: %v %s", err, r.body)
	}
	if list.Items[0].FailedDeliveries != 1 || list.Items[0].Target != rc.srv.URL {
		t.Errorf("webhook = %+v, want 1 failed and target %s", list.Items[0], rc.srv.URL)
	}
	for _, leak := range []string{hookPath, "t0ken", hookSecret} {
		if strings.Contains(string(r.body), leak) || strings.Contains(d.stringified(), leak) {
			t.Errorf("the API shows %q", leak)
		}
	}
}

func (d deliveryItem) stringified() string {
	b, _ := json.Marshal(d)
	return string(b)
}

func TestNotifications_RedeliverCreatesNewDelivery(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, _ := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStack(t, db, stub, rc, 3, memstore.New())

	digest := s.push("docker/demo/app", "1.0", "notify-redeliver")
	s.promoteTo(digest, "dev")
	eventually(t, "the first delivery", func() bool { return len(s.deliveries("?state=delivered")) == 1 })
	first := s.deliveries("")[0]

	r := s.request(http.MethodPost, fmt.Sprintf("/api/v1/notifications/deliveries/%d/redeliver", first.ID), nil, bearer(bootstrapToken))
	if r.status != http.StatusAccepted {
		t.Fatalf("redeliver: %d %s, want 202", r.status, r.body)
	}
	var second deliveryItem
	if err := json.Unmarshal(r.body, &second); err != nil || second.ID == first.ID || second.Event != "lifecycle.promoted" {
		t.Fatalf("redelivery = %+v (%v), want a new row for the same event", second, err)
	}
	eventually(t, "the second delivery", func() bool { return len(s.deliveries("?state=delivered")) == 2 })
	got := rc.all()
	if len(got) != 2 || !got[1].signed || got[1].envelope.ID != strconv.FormatInt(second.ID, 10) {
		t.Fatalf("receiver saw %d deliveries; second = %+v, want a signed one carrying id %d", len(got), got[len(got)-1].envelope, second.ID)
	}
	if got[1].envelope.Subject != got[0].envelope.Subject {
		t.Errorf("subjects differ: %+v vs %+v", got[0].envelope.Subject, got[1].envelope.Subject)
	}

	one := s.request(http.MethodGet, fmt.Sprintf("/api/v1/notifications/deliveries/%d", first.ID), nil, bearer(bootstrapToken))
	if one.status != http.StatusOK || !strings.Contains(string(one.body), `"payload":{`) {
		t.Errorf("GET one: %d %s, want the payload", one.status, one.body)
	}
	if r := s.request(http.MethodPost, "/api/v1/notifications/deliveries/999999/redeliver", nil, bearer(bootstrapToken)); r.status != http.StatusNotFound {
		t.Errorf("redeliver unknown id: %d, want 404", r.status)
	}
}

// promoteIn promotes subject to stage in the named repository.
func (s *stack) promoteIn(subject, repository, stage string) {
	s.t.Helper()
	r := s.request(http.MethodPost, "/api/v1/subjects/"+subject+"/promote",
		strings.NewReader(fmt.Sprintf(`{"repository":%q,"stage":%q,"reason":"repo test"}`, repository, stage)), bearer(bootstrapToken))
	if r.status != http.StatusCreated {
		s.t.Fatalf("promote in %s: %d %s", repository, r.status, r.body)
	}
}

// findingsSeen returns the finding.new envelopes the receiver holds.
func findingsSeen(rc *receiver) []received {
	var out []received
	for _, in := range rc.all() {
		if in.envelope.Event == "finding.new" {
			out = append(out, in)
		}
	}
	return out
}

func TestNotifications_FindingGoesToTheRepositoryThatApprovedTheDigest(t *testing.T) {
	// The digest sits in app-staging and prod but is approved only in prod. A
	// re-scan reads from app-staging, the lowest-named owner; the webhook
	// that watches prod must still hear about the finding, once, for prod.
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStackRepos(t, db, stub, rc, 3, memstore.New(), []string{"app-staging", "prod"}, "[prod]")

	digest := s.push("app-staging/demo/app", "1.0", "notify-two-repos")
	if again := s.push("prod/demo/app", "1.0", "notify-two-repos"); again != digest {
		t.Fatalf("the same content has two digests: %s, %s", digest, again)
	}
	eventually(t, "the first scan", func() bool { return len(s.scans(digest)) >= 1 })
	awaitScansFinished(t, db, 1)
	s.promoteIn(digest, "prod", "dev")
	eventually(t, "the promotion webhook", func() bool { return len(rc.all()) == 1 })

	newerDatabase()
	if r := s.request(http.MethodPost, "/api/v1/subjects/"+digest+"/scan", nil, bearer(bootstrapToken)); r.status != http.StatusAccepted {
		t.Fatalf("POST scan: %d %s", r.status, r.body)
	}
	eventually(t, "the finding webhook", func() bool { return len(findingsSeen(rc)) >= 1 })
	time.Sleep(300 * time.Millisecond)

	got := findingsSeen(rc)
	if len(got) != 1 {
		t.Fatalf("receiver saw %d finding.new deliveries, want 1", len(got))
	}
	var data notify.FindingData
	if err := json.Unmarshal(got[0].envelope.Data, &data); err != nil {
		t.Fatal(err)
	}
	if got[0].envelope.Subject.Repository != "prod" || len(data.ApprovedStages) != 1 || data.ApprovedStages[0] != "dev" {
		t.Errorf("subject = %+v, stages = %v, want prod and [dev]", got[0].envelope.Subject, data.ApprovedStages)
	}
}

func TestNotifications_FindingIsSentOncePerApprovingRepository(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	rc := newReceiver(t, http.StatusOK)
	s := notifyStackRepos(t, db, stub, rc, 3, memstore.New(), []string{"app-staging", "prod"}, "")

	digest := s.push("app-staging/demo/app", "1.0", "notify-both-repos")
	s.push("prod/demo/app", "1.0", "notify-both-repos")
	eventually(t, "the first scan", func() bool { return len(s.scans(digest)) >= 1 })
	awaitScansFinished(t, db, 1)
	s.promoteIn(digest, "prod", "dev")
	s.promoteIn(digest, "app-staging", "dev")
	eventually(t, "both promotion webhooks", func() bool { return len(rc.all()) == 2 })

	newerDatabase()
	if r := s.request(http.MethodPost, "/api/v1/subjects/"+digest+"/scan", nil, bearer(bootstrapToken)); r.status != http.StatusAccepted {
		t.Fatalf("POST scan: %d %s", r.status, r.body)
	}
	eventually(t, "both finding webhooks", func() bool { return len(findingsSeen(rc)) >= 2 })
	time.Sleep(300 * time.Millisecond)

	var repos []string
	for _, in := range findingsSeen(rc) {
		repos = append(repos, in.envelope.Subject.Repository)
	}
	sort.Strings(repos)
	if fmt.Sprint(repos) != "[app-staging prod]" {
		t.Errorf("finding.new repositories = %v, want one each for app-staging and prod", repos)
	}
}
