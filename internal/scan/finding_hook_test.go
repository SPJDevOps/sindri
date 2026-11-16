package scan

import (
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
)

func TestFindingHook_NeedsAWebhookForFindingNew(t *testing.T) {
	digest, err := model.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	newVulns := []string{"CVE-2026-1"}
	tests := []struct {
		name     string
		notifier *notify.Notifier
		vulns    *[]string
		wantHook bool
	}{
		{name: "NoNotifierNoHook", notifier: nil, vulns: &newVulns},
		{name: "NoWebhooksNoHook", notifier: notify.New(notify.Options{}), vulns: &newVulns},
		{name: "OnlyPromotedWebhookNoHook", notifier: notify.New(notify.Options{Webhooks: []notify.Webhook{{Name: "a", Events: []string{notify.EventPromoted}}}}), vulns: &newVulns},
		{name: "FindingWebhookHook", notifier: notify.New(notify.Options{Webhooks: []notify.Webhook{{Name: "a", Events: []string{notify.EventFindingNew}}}}), vulns: &newVulns, wantHook: true},
		{name: "NothingNewNoHook", notifier: notify.New(notify.Options{Webhooks: []notify.Webhook{{Name: "a", Events: []string{notify.EventFindingNew}}}}), vulns: &[]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Scanner{notifier: tt.notifier}
			if got := s.findingHook(digest, scanned{}, tt.vulns) != nil; got != tt.wantHook {
				t.Errorf("hook present = %t, want %t", got, tt.wantHook)
			}
		})
	}
}
