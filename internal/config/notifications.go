package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The events a webhook may subscribe to (ADR 34). internal/notify owns the
// same names and must not import this package; a test keeps the two in step.
const (
	// EventFindingNew is a new vulnerability found on a subject approved at
	// some stage.
	EventFindingNew = "finding.new"
	// EventPromoted is a promotion that committed.
	EventPromoted = "lifecycle.promoted"
)

// WebhookEvents lists every event name a webhook may subscribe to.
var WebhookEvents = []string{EventFindingNew, EventPromoted}

const (
	minWebhookSecretBytes = 32
	maxWebhookTimeout     = time.Minute
	defaultWebhookTimeout = 10 * time.Second
	minMaxAttempts        = 1
	maxMaxAttempts        = 50
	maxWebhookNameLen     = 63
)

var webhookNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Notifications configures outgoing webhooks (ADR 34). It is a server-level
// section with no manifest form, like scanning.
type Notifications struct {
	// The receivers to notify. Each one names the events it wants, and may be
	// limited to some repositories. Empty by default: nothing is sent.
	Webhooks []Webhook `yaml:"webhooks"`
	// How many times one delivery is attempted before it is recorded as failed,
	// with the queue's exponential backoff between attempts. Between 1 and 50.
	MaxAttempts int `yaml:"maxAttempts"`
}

// Webhook is one receiver.
type Webhook struct {
	// The webhook's name, which keys its delivery log. Lower-case letters,
	// digits, and hyphens, at most 63 characters, and unique in the list.
	Name string `yaml:"name"`
	// Where deliveries are POSTed. https, or http for a loopback address, and
	// without credentials in the URL. `config show`, the API, and the UI print
	// only its scheme and host, because the path often carries a token.
	URL string `yaml:"url"`
	// The key that signs each delivery with HMAC-SHA256. At least 32 bytes;
	// use `${VAR}` to keep it out of the file. Redacted by `config show`.
	Secret string `yaml:"secret"`
	// The events to send: `finding.new`, `lifecycle.promoted`, or both. At
	// least one, with no repeats.
	Events []string `yaml:"events"`
	// Send only events for subjects in these repositories. Empty means every
	// repository.
	Repositories []string `yaml:"repositories"`
	// How long one delivery attempt may take before it is cancelled and
	// retried. More than 0 and at most `1m`. Default: `10s`.
	Timeout Duration `yaml:"timeout"`
}

func defaultNotifications() Notifications {
	return Notifications{MaxAttempts: 8}
}

// UnmarshalYAML accepts only the mapping.
func (n *Notifications) UnmarshalYAML(node *yaml.Node) error {
	type raw Notifications
	return strictDecode(node, (*raw)(n))
}

// UnmarshalYAML rejects an unknown key in a webhook.
func (w *Webhook) UnmarshalYAML(node *yaml.Node) error {
	type raw Webhook
	return strictDecode(node, (*raw)(w))
}

func (n *Notifications) applyDefaults() {
	if n.MaxAttempts == 0 {
		n.MaxAttempts = defaultNotifications().MaxAttempts
	}
	for i := range n.Webhooks {
		if n.Webhooks[i].Timeout == 0 {
			n.Webhooks[i].Timeout = Duration(defaultWebhookTimeout)
		}
	}
}

// validate never includes a URL or a secret in an error: both are secrets.
func (n *Notifications) validate() error {
	if n.MaxAttempts < minMaxAttempts || n.MaxAttempts > maxMaxAttempts {
		return fmt.Errorf("notifications.maxAttempts: %d is outside %d to %d", n.MaxAttempts, minMaxAttempts, maxMaxAttempts)
	}
	seen := map[string]bool{}
	for i := range n.Webhooks {
		if err := n.Webhooks[i].validate(i, seen); err != nil {
			return err
		}
	}
	return nil
}

func (w *Webhook) validate(i int, seen map[string]bool) error {
	field := func(name string) string { return fmt.Sprintf("notifications.webhooks[%d].%s", i, name) }

	if w.Name == "" {
		return fmt.Errorf("%s: required", field("name"))
	}
	if len(w.Name) > maxWebhookNameLen || !webhookNamePattern.MatchString(w.Name) {
		return fmt.Errorf("%s: %q must be lower-case letters, digits, and hyphens, at most %d characters", field("name"), w.Name, maxWebhookNameLen)
	}
	if seen[w.Name] {
		return fmt.Errorf("%s: %q is used by an earlier webhook", field("name"), w.Name)
	}
	seen[w.Name] = true

	if w.URL == "" {
		return fmt.Errorf("%s: required", field("url"))
	}
	// url.Parse's own errors quote their input, and a webhook URL is a
	// secret, so the message here is fixed.
	u, err := url.Parse(w.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: is not a URL", field("url"))
	}
	if u.User != nil {
		return fmt.Errorf("%s: must not contain credentials", field("url"))
	}
	if err := checkHTTPSOrLoopback(field("url"), u); err != nil {
		return err
	}

	if w.Secret == "" {
		return fmt.Errorf("%s: required", field("secret"))
	}
	if len(w.Secret) < minWebhookSecretBytes {
		return fmt.Errorf("%s: is %d bytes; use at least %d", field("secret"), len(w.Secret), minWebhookSecretBytes)
	}

	if len(w.Events) == 0 {
		return fmt.Errorf("%s: name at least one of %s", field("events"), strings.Join(WebhookEvents, ", "))
	}
	for j, e := range w.Events {
		if !slices.Contains(WebhookEvents, e) {
			return fmt.Errorf("%s: unknown event %q; use %s", field("events"), e, strings.Join(WebhookEvents, ", "))
		}
		if slices.Contains(w.Events[:j], e) {
			return fmt.Errorf("%s: %q is listed twice", field("events"), e)
		}
	}
	for _, r := range w.Repositories {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("%s: a repository name must not be empty", field("repositories"))
		}
	}

	if w.Timeout <= 0 || w.Timeout.Duration() > maxWebhookTimeout {
		return fmt.Errorf("%s: %s is outside more than 0 and at most %s", field("timeout"), w.Timeout, maxWebhookTimeout)
	}
	return nil
}
