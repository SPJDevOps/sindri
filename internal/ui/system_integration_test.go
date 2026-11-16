//go:build integration

package ui

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
)

const (
	hookPath   = "/services/T000/B000/x-secret-path-token"
	hookQuery  = "key=query-secret-9f3"
	hookSecret = "signing-secret-do-not-show"
)

// withWebhook configures one webhook on the UI's notifier. Its URL carries a
// token in the path and the query, which no page may show.
func (e *env) withWebhook() {
	e.t.Helper()
	e.h.opts.Notifier = notify.New(notify.Options{Webhooks: []notify.Webhook{{
		Name:   "chat",
		URL:    "https://hooks.example.com" + hookPath + "?" + hookQuery,
		Secret: []byte(hookSecret),
		Events: []string{notify.EventFindingNew, notify.EventPromoted},
	}}})
	e.h.opts.PublicURL = "https://sindri.example.com"
}

func (e *env) delivery(webhook, event string, d model.Digest, state string, attempts int, status int, lastErr string) int64 {
	e.t.Helper()
	var id int64
	err := e.pool.QueryRow(e.t.Context(), `
INSERT INTO webhook_deliveries (webhook, event, subject_digest, repository, payload, dedupe_key, state, attempts, last_status, last_error, created_at)
VALUES ($1, $2, $3, 'team-a', '{"secret-payload":"nope"}', $4, $5, $6, NULLIF($7, 0), NULLIF($8, ''), $9)
RETURNING id`,
		webhook, event, d.String(), fmt.Sprintf("%s:%s:%s:%d", event, d, state, attempts), state, attempts, status, lastErr, time.Now().Add(-time.Minute)).Scan(&id)
	if err != nil {
		e.t.Fatalf("seeding delivery: %v", err)
	}
	return id
}

func TestSystem_IsAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.withWebhook()
	for _, path := range []string{"/ui/system", "/ui/system/notifications"} {
		for _, tc := range []struct {
			user string
			want int
		}{{"alice", 404}, {"bob", 404}, {"", 404}, {"admin", 200}} {
			if r := e.get(tc.user, path); r.status != tc.want {
				t.Errorf("%s as %q: %d, want %d", path, tc.user, r.status, tc.want)
			}
		}
	}
	if r := e.get("admin", "/ui/"); !strings.Contains(r.body, `href="/ui/system"`) {
		t.Error("the System link is missing for an administrator")
	}
	for _, user := range []string{"alice", "bob"} {
		if r := e.get(user, "/ui/"); strings.Contains(r.body, "/ui/system") {
			t.Errorf("%s sees a link to /ui/system", user)
		}
	}
}

func TestSystem_WithoutNotificationsSaysSo(t *testing.T) {
	e := newEnv(t)
	r := e.get("admin", "/ui/system")
	if r.status != http.StatusOK || !strings.Contains(r.body, "No webhooks configured") {
		t.Fatalf("status %d, body lacks the empty state", r.status)
	}
}

var statusLink = regexp.MustCompile(`class="status-line[^"]*"[^>]*href="([^"]+)"|href="([^"]+)"[^>]*class="status-line`)

// SIN-112 acceptance: a delivery that failed three times is two clicks from
// /system, with the response status shown.
func TestSystem_AFailedDeliveryIsTwoClicksAway(t *testing.T) {
	e := newEnv(t)
	e.withWebhook()
	d := e.push("team-a", "app", "1.0", "sys")
	okID := e.delivery("chat", notify.EventPromoted, d, "delivered", 1, 200, "")
	failedID := e.delivery("chat", notify.EventFindingNew, d, "failed", 3, 503, "receiver chat answered 503")

	home := e.get("admin", "/ui/system")
	if home.status != http.StatusOK {
		t.Fatalf("/ui/system: %d", home.status)
	}
	if !strings.Contains(home.body, "1 webhook failing") {
		t.Fatalf("status line missing from /ui/system:\n%s", home.body)
	}
	m := statusLink.FindStringSubmatch(home.body)
	if m == nil {
		t.Fatalf("the status line is not a link:\n%s", home.body)
	}
	link := m[1] + m[2]
	log := e.get("admin", link)
	if log.status != http.StatusOK {
		t.Fatalf("%s: %d", link, log.status)
	}
	for _, want := range []string{
		"503", "receiver chat answered 503", "finding.new", "failed",
		fmt.Sprintf("/api/v1/notifications/deliveries/%d/redeliver", failedID),
		"curl -X POST", `Authorization: Bearer $TOKEN`, "https://sindri.example.com",
	} {
		if !strings.Contains(log.body, want) {
			t.Errorf("filtered log lacks %q", want)
		}
	}
	if strings.Contains(log.body, fmt.Sprintf("deliveries/%d/redeliver", okID)) {
		t.Error("the failed filter shows a delivered row")
	}
	// The unfiltered log shows both, and the delivered row has no command.
	all := e.get("admin", "/ui/system/notifications")
	if !strings.Contains(all.body, "lifecycle.promoted") || !strings.Contains(all.body, "finding.new") {
		t.Error("the unfiltered log lacks a row")
	}
	if strings.Contains(all.body, fmt.Sprintf("deliveries/%d/redeliver", okID)) {
		t.Error("a delivered row offers redelivery")
	}
	// The row links to its artifact page.
	if !strings.Contains(log.body, "/ui/r/team-a/-/artifact?digest=") {
		t.Error("the subject does not link to its artifact page")
	}
}

func TestSystem_NeverRendersWebhookURLPartsOrSecret(t *testing.T) {
	e := newEnv(t)
	e.withWebhook()
	d := e.push("team-a", "app", "1.0", "redact")
	e.delivery("chat", notify.EventFindingNew, d, "failed", 3, 500, "receiver chat answered 500")
	e.delivery("chat", notify.EventPromoted, d, "pending", 1, 502, "receiver chat answered 502")

	pages := []string{
		"/ui/system", "/ui/system/notifications", "/ui/system/notifications?webhook=chat&state=failed",
		"/ui/r/team-a/-/artifact?digest=" + d.String(),
	}
	for _, path := range pages {
		r := e.get("admin", path)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d", path, r.status)
		}
		for _, leak := range []string{hookPath, "x-secret-path-token", hookQuery, "query-secret", hookSecret, "secret-payload"} {
			if strings.Contains(r.body, leak) {
				t.Errorf("%s renders %q", path, leak)
			}
		}
	}
	if r := e.get("admin", "/ui/system"); !strings.Contains(r.body, "https://hooks.example.com") {
		t.Error("/ui/system no longer names the target host")
	}
}

func TestArtifactPage_ShowsDeliveriesToAdministratorsOnly(t *testing.T) {
	e := newEnv(t)
	e.withWebhook()
	d := e.push("team-a", "app", "1.0", "timeline")
	e.delivery("chat", notify.EventFindingNew, d, "delivered", 1, 200, "")

	admin := e.get("admin", "/ui/r/team-a/-/artifact?digest="+d.String())
	if !strings.Contains(admin.body, "Notification sent to") || !strings.Contains(admin.body, "chat") || !strings.Contains(admin.body, "finding.new") {
		t.Errorf("an administrator does not see the delivery:\n%s", admin.body)
	}
	alice := e.get("alice", "/ui/r/team-a/-/artifact?digest="+d.String())
	if alice.status != http.StatusOK {
		t.Fatalf("alice: %d", alice.status)
	}
	for _, leak := range []string{"Notification sent", "chat", "finding.new"} {
		if strings.Contains(alice.body, leak) {
			t.Errorf("a non-administrator sees %q on the artifact page", leak)
		}
	}
}
