package main

import (
	"testing"
	"time"

	"github.com/spj/sindri/internal/config"
)

func TestWebhooksFromConfig_CopiesEveryField(t *testing.T) {
	got := webhooksFromConfig(config.Notifications{Webhooks: []config.Webhook{{
		Name: "a", URL: "https://h.example/x", Secret: "s", Events: []string{"finding.new"},
		Repositories: []string{"docker"}, Timeout: config.Duration(5 * time.Second),
	}}})
	if len(got) != 1 || got[0].Name != "a" || got[0].URL != "https://h.example/x" || string(got[0].Secret) != "s" ||
		got[0].Timeout != 5*time.Second || got[0].Repositories[0] != "docker" || got[0].Events[0] != "finding.new" {
		t.Errorf("webhooksFromConfig = %+v", got)
	}
}
