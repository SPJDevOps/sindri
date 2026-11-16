package main

import (
	"slices"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/notify"
)

// webhooksFromConfig builds the webhook set from the notifications section.
// It lives here, not in internal/notify, so that notify does not depend on
// config and, through it, on the registry.
func webhooksFromConfig(c config.Notifications) []notify.Webhook {
	out := make([]notify.Webhook, 0, len(c.Webhooks))
	for _, w := range c.Webhooks {
		out = append(out, notify.Webhook{
			Name:         w.Name,
			URL:          w.URL,
			Secret:       []byte(w.Secret),
			Events:       slices.Clone(w.Events),
			Repositories: slices.Clone(w.Repositories),
			Timeout:      w.Timeout.Duration(),
		})
	}
	return out
}
