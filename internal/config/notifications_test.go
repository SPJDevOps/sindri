package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/config"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func webhookYAML(extra string) string {
	return "notifications:\n  webhooks:\n    - name: sec\n      url: https://hooks.example/x\n      secret: " + goodSecret + "\n      events: [finding.new]\n" + extra
}

func TestParse_NotificationsDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte(webhookYAML("")), "sindri.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := config.Notifications{
		MaxAttempts: 8,
		Webhooks: []config.Webhook{{
			Name: "sec", URL: "https://hooks.example/x", Secret: goodSecret,
			Events: []string{"finding.new"}, Timeout: config.Duration(10 * time.Second),
		}},
	}
	if diff := cmp.Diff(want, cfg.Notifications); diff != "" {
		t.Errorf("notifications (-want +got):\n%s", diff)
	}
	if got := config.Default().Notifications.MaxAttempts; got != 8 {
		t.Errorf("Default maxAttempts = %d, want 8", got)
	}
}

func TestParse_RejectsNotifications(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		want       string
		wantAbsent string
	}{
		{name: "NameMissing", input: strings.Replace(webhookYAML(""), "name: sec", "name: \"\"", 1), want: "notifications.webhooks[0].name"},
		{name: "NameNotASlug", input: strings.Replace(webhookYAML(""), "name: sec", "name: Not_A_Slug", 1), want: "notifications.webhooks[0].name"},
		{name: "NameDuplicate", input: webhookYAML("    - name: sec\n      url: https://b.example/x\n      secret: " + goodSecret + "\n      events: [finding.new]\n"), want: "notifications.webhooks[1].name"},
		{name: "URLNotHTTPS", input: strings.Replace(webhookYAML(""), "https://hooks.example/x", "http://hooks.example/tok3n", 1), want: "notifications.webhooks[0].url", wantAbsent: "tok3n"},
		{name: "URLWithUserinfo", input: strings.Replace(webhookYAML(""), "https://hooks.example/x", "https://user:tok3n@hooks.example/x", 1), want: "notifications.webhooks[0].url", wantAbsent: "tok3n"},
		{name: "URLUsernameOnly", input: strings.Replace(webhookYAML(""), "https://hooks.example/x", "https://tok3n@hooks.example/x", 1), want: "must not contain credentials", wantAbsent: "tok3n"},
		{name: "URLUnparseable", input: strings.Replace(webhookYAML(""), "https://hooks.example/x", "\"https://tok3n:%zz@hooks.example\"", 1), want: "notifications.webhooks[0].url", wantAbsent: "tok3n"},
		{name: "SecretMissing", input: strings.Replace(webhookYAML(""), "secret: "+goodSecret, "secret: \"\"", 1), want: "notifications.webhooks[0].secret"},
		{name: "SecretTooShort", input: strings.Replace(webhookYAML(""), "secret: "+goodSecret, "secret: shortsecret", 1), want: "notifications.webhooks[0].secret", wantAbsent: "shortsecret"},
		{name: "EventsEmpty", input: strings.Replace(webhookYAML(""), "events: [finding.new]", "events: []", 1), want: "notifications.webhooks[0].events"},
		{name: "EventUnknown", input: strings.Replace(webhookYAML(""), "[finding.new]", "[finding.old]", 1), want: "notifications.webhooks[0].events"},
		{name: "EventDuplicate", input: strings.Replace(webhookYAML(""), "[finding.new]", "[finding.new, finding.new]", 1), want: "notifications.webhooks[0].events"},
		{name: "RepositoryEmptyName", input: webhookYAML("      repositories: [\"\"]\n"), want: "notifications.webhooks[0].repositories"},
		{name: "TimeoutTooLong", input: webhookYAML("      timeout: 2m\n"), want: "notifications.webhooks[0].timeout"},
		{name: "UnknownWebhookKey", input: webhookYAML("      colour: red\n"), want: `unknown field "colour"`},
		{name: "MaxAttemptsTooHigh", input: "notifications: {maxAttempts: 51}\n", want: "notifications.maxAttempts"},
		{name: "MaxAttemptsNegative", input: "notifications: {maxAttempts: -1}\n", want: "notifications.maxAttempts"},
		{name: "UnknownKey", input: "notifications: {hooks: []}\n", want: `unknown field "hooks"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.input), "sindri.yaml")
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			if tc.wantAbsent != "" && strings.Contains(err.Error(), tc.wantAbsent) {
				t.Errorf("error %q echoes %q", err, tc.wantAbsent)
			}
			if strings.Contains(err.Error(), goodSecret) {
				t.Errorf("error %q echoes the secret", err)
			}
		})
	}
}

func TestParse_NotificationsAcceptsLoopbackHTTPAndFilters(t *testing.T) {
	in := strings.Replace(webhookYAML("      repositories: [docker, pypi]\n      timeout: 30s\n"), "https://hooks.example/x", "http://127.0.0.1:9000/x", 1)
	in = strings.Replace(in, "[finding.new]", "[finding.new, lifecycle.promoted]", 1) + "  maxAttempts: 3\n"
	cfg, err := config.Parse([]byte(in), "sindri.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Notifications.Webhooks[0]
	if cfg.Notifications.MaxAttempts != 3 || w.Timeout.Duration() != 30*time.Second || len(w.Repositories) != 2 || len(w.Events) != 2 {
		t.Errorf("unexpected notifications: %+v", cfg.Notifications)
	}
}
