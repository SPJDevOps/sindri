package ui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
)

// The system pages are for administrators: webhook names, targets, and the
// delivery log are operator data, not something a repository's readers should
// learn. Anyone else gets the ordinary 404 page, so the route's existence is
// not confirmed either (ADR 34).

const (
	systemHref        = Prefix + "/system"
	notificationsHref = Prefix + "/system/notifications"

	// deliveryLogLimit is how many deliveries the log shows, newest first.
	deliveryLogLimit = 100
	// artifactDeliveryLimit is how many an artifact page lists.
	artifactDeliveryLimit = 20
)

// examplePayloads are static, documented envelopes. They are not live bodies:
// a stored payload never reaches a page.
var examplePayloads = []examplePayload{
	{Event: "finding.new", Body: `{
  "id": "1042",
  "event": "finding.new",
  "occurredAt": "2026-11-16T10:00:00Z",
  "sindri": {"url": "https://sindri.example", "version": "1.6.0"},
  "subject": {"digest": "sha256:4b825dc642cb6eb9a060e54bf8d69288fbee4904cafebabedeadbeefcafebabe", "repository": "docker"},
  "data": {
    "newVulnerabilities": ["CVE-2026-0001"],
    "severityCounts": {"CRITICAL": 1, "HIGH": 0, "MEDIUM": 2, "LOW": 0, "UNKNOWN": 0},
    "platform": "linux/amd64",
    "approvedStages": ["prod"],
    "evidenceId": "0b8e5f1c-5c1e-4a43-9a43-2a7a8a0f3d11"
  }
}`},
	{Event: "lifecycle.promoted", Body: `{
  "id": "1043",
  "event": "lifecycle.promoted",
  "occurredAt": "2026-11-16T10:05:00Z",
  "sindri": {"url": "https://sindri.example", "version": "1.6.0"},
  "subject": {"digest": "sha256:4b825dc642cb6eb9a060e54bf8d69288fbee4904cafebabedeadbeefcafebabe", "repository": "docker", "stage": "prod"},
  "data": {
    "actor": "user:alice",
    "reason": "release 2.4",
    "eventId": 317,
    "evidenceIds": ["0b8e5f1c-5c1e-4a43-9a43-2a7a8a0f3d11"]
  }
}`},
}

type examplePayload struct {
	Event string
	Body  string
}

type hookRow struct {
	Name    string
	Events  string
	Target  string
	Failed  int64
	LogHref string
}

type systemPage struct {
	chrome
	Configured bool
	// StatusClass is the status dot's state: ok, warn, or bad.
	StatusClass string
	StatusText  string
	StatusHref  string
	Hooks       []hookRow
	Examples    []examplePayload
}

// requireAdmin answers 404 to anyone but an administrator.
func (h *handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !principal(r).Admin {
		h.notFound(w, r)
		return false
	}
	return true
}

func (h *handler) queries() *db.Queries {
	if h.opts.NotifyDB == nil {
		return nil
	}
	return h.opts.NotifyDB.Queries()
}

func (h *handler) system(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	data := systemPage{
		chrome:   h.chrome(r, "System", crumb{"system", ""}),
		Examples: examplePayloads,
	}
	var hooks []notify.Webhook
	if h.opts.Notifier != nil {
		hooks = h.opts.Notifier.Webhooks()
	}
	failed := map[string]int64{}
	if q := h.queries(); q != nil {
		rows, err := q.CountFailedWebhookDeliveriesByWebhook(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, row := range rows {
			failed[row.Webhook] = row.Failed
		}
	}
	data.Configured = len(hooks) > 0
	for _, wh := range hooks {
		data.Hooks = append(data.Hooks, hookRow{
			Name:    wh.Name,
			Events:  strings.Join(wh.Events, ", "),
			Target:  model.URLHost(wh.URL),
			Failed:  failed[wh.Name],
			LogHref: logHref(wh.Name, ""),
		})
	}
	switch n := len(failed); {
	case n > 0:
		data.StatusClass = "bad"
		data.StatusText = plural(n, "webhook") + " failing"
		data.StatusHref = logHref("", "failed")
	case data.Configured:
		data.StatusClass = "ok"
		data.StatusText = "All webhooks delivering"
		data.StatusHref = logHref("", "")
	default:
		data.StatusClass = "none"
		data.StatusText = "No webhooks configured"
		data.StatusHref = logHref("", "")
	}
	h.render(w, r, "system", "", http.StatusOK, data)
}

// logHref is the delivery log, filtered by webhook and state when they are set.
func logHref(webhook, state string) string {
	q := url.Values{}
	if webhook != "" {
		q.Set("webhook", webhook)
	}
	if state != "" {
		q.Set("state", state)
	}
	if len(q) == 0 {
		return notificationsHref
	}
	return notificationsHref + "?" + q.Encode()
}

type deliveryRow struct {
	ID         int64
	Webhook    string
	Event      string
	Digest     string
	Href       string // the artifact page; empty when it is not linkable
	Repository string
	State      string
	Dot        string
	Status     string
	Attempts   int32
	At         time.Time
	NextRetry  time.Time
	Error      string
	Redeliver  string // a copyable command, set on failed rows only
}

type stateFilter struct {
	Label  string
	Href   string
	Active bool
}

type deliveriesPage struct {
	chrome
	Webhook string
	State   string
	Filters []stateFilter
	Rows    []deliveryRow
	Limit   int
	HasMore bool
	Clear   string
}

func dotFor(state string) string {
	switch state {
	case "delivered":
		return "ok"
	case "failed":
		return "bad"
	default:
		return "warn"
	}
}

func (h *handler) deliveryRows(ctx context.Context, rows []db.ListWebhookDeliveriesRow, link func(repo string, d model.Digest) string) []deliveryRow {
	out := make([]deliveryRow, 0, len(rows))
	for _, row := range rows {
		dr := deliveryRow{
			ID: row.ID, Webhook: row.Webhook, Event: row.Event, Digest: row.SubjectDigest,
			Repository: row.Repository, State: row.State, Dot: dotFor(row.State),
			Attempts: row.Attempts, At: row.CreatedAt, Error: row.LastError.String,
		}
		if row.LastStatus.Valid {
			dr.Status = strconv.Itoa(int(row.LastStatus.Int32))
		}
		if row.State == "pending" && row.JobRunAfter.Valid {
			dr.NextRetry = row.JobRunAfter.Time
		}
		if d, err := model.ParseDigest(row.SubjectDigest); err == nil && link != nil {
			dr.Href = link(row.Repository, d)
		}
		if row.State == "failed" {
			dr.Redeliver = h.redeliverCommand(row.ID)
		}
		out = append(out, dr)
	}
	return out
}

// redeliverCommand is what an administrator copies to redeliver; the UI never
// changes state itself.
func (h *handler) redeliverCommand(id int64) string {
	base := strings.TrimRight(h.opts.PublicURL, "/")
	if base == "" {
		base = "$SINDRI_URL"
	}
	return `curl -X POST -H "Authorization: Bearer $TOKEN" ` + base + "/api/v1/notifications/deliveries/" + strconv.FormatInt(id, 10) + "/redeliver"
}

// artifactLinker returns a link builder that only links a repository the
// caller may read, and remembers the answer for the request.
func (h *handler) artifactLinker(r *http.Request) func(repo string, d model.Digest) string {
	seen := map[string]bool{}
	return func(repo string, d model.Digest) string {
		ok, known := seen[repo]
		if !known {
			rp, err := h.opts.Registry.GetRepository(r.Context(), repo)
			ok = err == nil && readable(r, rp)
			seen[repo] = ok
		}
		if !ok {
			return ""
		}
		return artifactHref(repo, "", d)
	}
}

func (h *handler) notificationLog(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	webhook := r.URL.Query().Get("webhook")
	state := r.URL.Query().Get("state")
	switch state {
	case "pending", "delivered", "failed":
	default:
		state = ""
	}
	data := deliveriesPage{
		chrome:  h.chrome(r, "Notifications", crumb{"system", systemHref}, crumb{"notifications", ""}),
		Webhook: webhook, State: state, Limit: deliveryLogLimit,
	}
	for _, f := range []struct{ label, state string }{{"all", ""}, {"pending", "pending"}, {"delivered", "delivered"}, {"failed", "failed"}} {
		data.Filters = append(data.Filters, stateFilter{Label: f.label, Href: logHref(webhook, f.state), Active: f.state == state})
	}
	if webhook != "" {
		data.Clear = logHref("", state)
	}
	if q := h.queries(); q != nil {
		params := db.ListWebhookDeliveriesParams{RowLimit: deliveryLogLimit + 1}
		if webhook != "" {
			params.Webhook = pgtype.Text{String: webhook, Valid: true}
		}
		if state != "" {
			params.State = pgtype.Text{String: state, Valid: true}
		}
		rows, err := q.ListWebhookDeliveries(r.Context(), params)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if len(rows) > deliveryLogLimit {
			rows, data.HasMore = rows[:deliveryLogLimit], true
		}
		data.Rows = h.deliveryRows(r.Context(), rows, h.artifactLinker(r))
	}
	h.render(w, r, "deliveries", "", http.StatusOK, data)
}

// artifactDeliveries lists the deliveries about one subject, for an
// administrator's artifact page. Anyone else gets nothing, because the webhook
// names are operator data.
func (h *handler) artifactDeliveries(r *http.Request, digest model.Digest) ([]deliveryRow, error) {
	q := h.queries()
	if q == nil || !principal(r).Admin {
		return nil, nil
	}
	rows, err := q.ListWebhookDeliveries(r.Context(), db.ListWebhookDeliveriesParams{
		SubjectDigest: pgtype.Text{String: digest.String(), Valid: true},
		RowLimit:      artifactDeliveryLimit,
	})
	if err != nil {
		return nil, err
	}
	return h.deliveryRows(r.Context(), rows, nil), nil
}
