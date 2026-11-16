package notify_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/notify"
)

func TestSign_MatchesTheDocumentedVector(t *testing.T) {
	const want = "sha256=6978ad556b67ac14d4a32643e5ac800f0a50d97691f4379d653ca60e4bbbc600"
	got := notify.Sign([]byte("0123456789abcdef0123456789abcdef"), 1700000000, []byte(`{"event":"finding.new"}`))
	if got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
}

func TestSign_ChangesWithTimestampBodyAndSecret(t *testing.T) {
	base := notify.Sign([]byte("k"), 1, []byte("b"))
	for name, other := range map[string]string{
		"Timestamp": notify.Sign([]byte("k"), 2, []byte("b")),
		"Body":      notify.Sign([]byte("k"), 1, []byte("c")),
		"Secret":    notify.Sign([]byte("j"), 1, []byte("b")),
	} {
		if other == base {
			t.Errorf("changing the %s did not change the signature", name)
		}
	}
}

func TestDecide_StatusCodesMapToAnOutcome(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   notify.Outcome
	}{
		{name: "200IsDelivered", status: 200, want: notify.Delivered},
		{name: "204IsDelivered", status: 204, want: notify.Delivered},
		{name: "299IsDelivered", status: 299, want: notify.Delivered},
		{name: "408Retries", status: 408, want: notify.Retry},
		{name: "429Retries", status: 429, want: notify.Retry},
		{name: "500Retries", status: 500, want: notify.Retry},
		{name: "503Retries", status: 503, want: notify.Retry},
		{name: "301IsFinal", status: 301, want: notify.Final},
		{name: "302IsFinal", status: 302, want: notify.Final},
		{name: "400IsFinal", status: 400, want: notify.Final},
		{name: "401IsFinal", status: 401, want: notify.Final},
		{name: "404IsFinal", status: 404, want: notify.Final},
		{name: "410IsFinal", status: 410, want: notify.Final},
		{name: "NetworkErrorRetries", err: errors.New("connection refused"), want: notify.Retry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notify.Decide(tc.status, tc.err); got != tc.want {
				t.Errorf("Decide(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

func TestWebhook_SubscribesByEventAndRepository(t *testing.T) {
	tests := []struct {
		name  string
		hook  notify.Webhook
		event string
		repo  string
		want  bool
	}{
		{name: "EventAndAnyRepository", hook: notify.Webhook{Events: []string{"finding.new"}}, event: "finding.new", repo: "docker", want: true},
		{name: "OtherEvent", hook: notify.Webhook{Events: []string{"finding.new"}}, event: "lifecycle.promoted", repo: "docker", want: false},
		{name: "ListedRepository", hook: notify.Webhook{Events: []string{"finding.new"}, Repositories: []string{"pypi", "docker"}}, event: "finding.new", repo: "docker", want: true},
		{name: "UnlistedRepository", hook: notify.Webhook{Events: []string{"finding.new"}, Repositories: []string{"pypi"}}, event: "finding.new", repo: "docker", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hook.Subscribes(tc.event, tc.repo); got != tc.want {
				t.Errorf("Subscribes(%q, %q) = %t, want %t", tc.event, tc.repo, got, tc.want)
			}
		})
	}
}

func TestNotifier_SwapReplacesTheSet(t *testing.T) {
	n := notify.New(notify.Options{})
	if len(n.Webhooks()) != 0 {
		t.Fatal("a new notifier has webhooks")
	}
	n.Swap([]notify.Webhook{{Name: "a"}})
	if got := n.Webhooks(); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("Webhooks = %+v after Swap", got)
	}
	if _, ok := n.Webhook("a"); !ok {
		t.Error("Webhook(a) not found")
	}
	if _, ok := n.Webhook("b"); ok {
		t.Error("Webhook(b) found")
	}
}

func TestEnvelope_MarshalsEmptyListsNotNull(t *testing.T) {
	for name, data := range map[string]notify.EventData{
		"Finding":  notify.FindingData{},
		"Promoted": notify.PromotedData{},
	} {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "null") {
			t.Errorf("%s marshals to %s, which has a null", name, raw)
		}
	}
}
