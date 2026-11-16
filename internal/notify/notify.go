// Package notify sends webhooks for security-relevant events (ADR 34).
//
// Emitting an event inserts one webhook_deliveries row per matching webhook
// and enqueues a deliver_webhook job, in the caller's transaction where there
// is one. Nothing is sent on the caller's path. The Deliverer is the job's
// handler: it signs, POSTs, records the outcome, and decides whether the queue
// retries.
//
// The package depends on db, queue, model, config, and worker, so lifecycle
// and scan can call it without a cycle.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
)

// The event names, as they appear in configuration and in payloads.
const (
	EventFindingNew = config.EventFindingNew
	EventPromoted   = config.EventPromoted
)

// JobKind is the queue kind of one delivery.
const JobKind = "deliver_webhook"

// ErrUnknownWebhook means a delivery names a webhook that is not configured.
var ErrUnknownWebhook = errors.New("webhook is not configured")

// ErrDeliveryNotFound means no delivery has the id.
var ErrDeliveryNotFound = errors.New("webhook delivery not found")

// Webhook is one configured receiver.
type Webhook struct {
	Name string
	// URL and Secret are secrets: never log, print, or return them.
	URL          string
	Secret       []byte
	Events       []string
	Repositories []string
	Timeout      time.Duration
}

// Subscribes reports whether the webhook wants an event of this kind for a
// subject in this repository.
func (w Webhook) Subscribes(event, repository string) bool {
	if !slices.Contains(w.Events, event) {
		return false
	}
	return len(w.Repositories) == 0 || slices.Contains(w.Repositories, repository)
}

// FromConfig builds the webhook set from the notifications section.
func FromConfig(c config.Notifications) []Webhook {
	out := make([]Webhook, 0, len(c.Webhooks))
	for _, w := range c.Webhooks {
		out = append(out, Webhook{
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

// EventData is the typed `data` of one event kind.
type EventData interface {
	// EventKind is the event name the data belongs to.
	EventKind() string
}

// FindingData is the data of finding.new: one re-scan result that found
// vulnerabilities not seen before on a subject approved at some stage.
type FindingData struct {
	NewVulnerabilities []string       `json:"newVulnerabilities"`
	SeverityCounts     map[string]int `json:"severityCounts"`
	Platform           string         `json:"platform"`
	ApprovedStages     []string       `json:"approvedStages"`
	EvidenceID         string         `json:"evidenceId"`
}

// EventKind implements EventData.
func (FindingData) EventKind() string { return EventFindingNew }

// MarshalJSON writes empty lists and maps as [] and {}, never null, so a
// receiver need not tell absent from empty.
func (d FindingData) MarshalJSON() ([]byte, error) {
	type plain FindingData
	p := plain(d)
	if p.NewVulnerabilities == nil {
		p.NewVulnerabilities = []string{}
	}
	if p.SeverityCounts == nil {
		p.SeverityCounts = map[string]int{}
	}
	if p.ApprovedStages == nil {
		p.ApprovedStages = []string{}
	}
	return json.Marshal(p)
}

// PromotedData is the data of lifecycle.promoted.
type PromotedData struct {
	Actor       string   `json:"actor"`
	Reason      string   `json:"reason"`
	EventID     int64    `json:"eventId"`
	EvidenceIDs []string `json:"evidenceIds"`
}

// EventKind implements EventData.
func (PromotedData) EventKind() string { return EventPromoted }

// MarshalJSON writes an empty evidence list as [], never null.
func (d PromotedData) MarshalJSON() ([]byte, error) {
	type plain PromotedData
	p := plain(d)
	if p.EvidenceIDs == nil {
		p.EvidenceIDs = []string{}
	}
	return json.Marshal(p)
}

// Event is one thing worth telling a receiver.
type Event struct {
	Digest     model.Digest
	Repository string
	// Stage is the stage the event concerns. Empty for finding.new, which
	// concerns the subject at every stage it is approved for.
	Stage string
	// DedupeKey is unique per event and stable across retries of whatever
	// emitted it, for example `finding:<evidence id>` or `promoted:<ledger
	// id>`. A webhook that already holds a delivery with the key is skipped.
	DedupeKey string
	// OccurredAt defaults to now.
	OccurredAt time.Time
	Data       EventData
}

// Kind is the event name.
func (e Event) Kind() string {
	if e.Data == nil {
		return ""
	}
	return e.Data.EventKind()
}

// Envelope is the JSON body of every delivery.
type Envelope struct {
	ID         string          `json:"id"`
	Event      string          `json:"event"`
	OccurredAt time.Time       `json:"occurredAt"`
	Sindri     SindriInfo      `json:"sindri"`
	Subject    Subject         `json:"subject"`
	Data       json.RawMessage `json:"data"`
}

// SindriInfo says which server sent the delivery.
type SindriInfo struct {
	URL     string `json:"url"`
	Version string `json:"version"`
}

// Subject is what the event is about.
type Subject struct {
	Digest     string `json:"digest"`
	Repository string `json:"repository"`
	Stage      string `json:"stage,omitempty"`
}

// Options configures a Notifier.
type Options struct {
	Webhooks    []Webhook
	MaxAttempts int
	// PublicURL and Version go in every envelope's `sindri` object.
	PublicURL string
	Version   string
	// Now defaults to time.Now.
	Now func() time.Time
}

// DefaultMaxAttempts matches the configuration default.
const DefaultMaxAttempts = 8

// Notifier emits events. The webhook set is swapped atomically on SIGHUP.
type Notifier struct {
	webhooks    atomic.Pointer[[]Webhook]
	maxAttempts atomic.Int32
	publicURL   string
	version     string
	now         func() time.Time
}

// New returns a Notifier holding opts.Webhooks.
func New(opts Options) *Notifier {
	n := &Notifier{publicURL: opts.PublicURL, version: opts.Version, now: opts.Now}
	if n.now == nil {
		n.now = time.Now
	}
	n.Swap(opts.Webhooks)
	n.SetMaxAttempts(opts.MaxAttempts)
	return n
}

// Swap replaces the webhook set. An emit or delivery in flight sees either the
// old set or the new one.
func (n *Notifier) Swap(ws []Webhook) {
	cp := slices.Clone(ws)
	n.webhooks.Store(&cp)
}

// SetMaxAttempts replaces the attempts a new delivery's job is given. A value
// below 1 means DefaultMaxAttempts.
func (n *Notifier) SetMaxAttempts(v int) {
	if v < 1 {
		v = DefaultMaxAttempts
	}
	n.maxAttempts.Store(int32(min(v, 1<<20))) //nolint:gosec // clamped above
}

// Webhooks returns the current set. The slice is the caller's to keep.
func (n *Notifier) Webhooks() []Webhook { return slices.Clone(*n.webhooks.Load()) }

// Webhook finds a configured webhook by name.
func (n *Notifier) Webhook(name string) (Webhook, bool) {
	for _, w := range *n.webhooks.Load() {
		if w.Name == name {
			return w, true
		}
	}
	return Webhook{}, false
}

// EmitTx records one delivery per webhook that subscribes to ev, and enqueues
// a job for each, on q, which is bound to the caller's transaction: the
// deliveries exist exactly when the caller's work commits. It returns how many
// deliveries were created; a webhook that already holds the event's dedupe
// key is skipped, and an event nobody subscribes to runs no query.
//
// A delivery's envelope id is its row id, which exists only after the insert,
// so the row is inserted with an empty payload and updated in the same
// transaction.
func (n *Notifier) EmitTx(ctx context.Context, q *db.Queries, ev Event) (int, error) {
	kind := ev.Kind()
	if kind == "" || !ev.Digest.Valid() || ev.DedupeKey == "" {
		return 0, fmt.Errorf("emitting an event: it needs data, a digest, and a dedupe key")
	}
	var targets []Webhook
	for _, w := range *n.webhooks.Load() {
		if w.Subscribes(kind, ev.Repository) {
			targets = append(targets, w)
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}

	data, err := json.Marshal(ev.Data)
	if err != nil {
		return 0, fmt.Errorf("marshalling %s data: %w", kind, err)
	}
	occurred := ev.OccurredAt
	if occurred.IsZero() {
		occurred = n.now()
	}
	env := Envelope{
		Event:      kind,
		OccurredAt: occurred.UTC(),
		Sindri:     SindriInfo{URL: n.publicURL, Version: n.version},
		Subject:    Subject{Digest: ev.Digest.String(), Repository: ev.Repository, Stage: ev.Stage},
		Data:       data,
	}

	created := 0
	for _, w := range targets {
		ok, err := n.insertDelivery(ctx, q, w.Name, env, ev.DedupeKey)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// insertDelivery writes one delivery row and its job. It reports false when
// the webhook already holds the dedupe key.
func (n *Notifier) insertDelivery(ctx context.Context, q *db.Queries, webhook string, env Envelope, dedupeKey string) (bool, error) {
	row, err := q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{
		Webhook:       webhook,
		Event:         env.Event,
		SubjectDigest: env.Subject.Digest,
		Repository:    env.Subject.Repository,
		Payload:       []byte(`{}`),
		DedupeKey:     dedupeKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recording a %s delivery for webhook %s: %w", env.Event, webhook, err)
	}

	env.ID = fmt.Sprint(row.ID)
	payload, err := json.Marshal(env)
	if err != nil {
		return false, fmt.Errorf("marshalling the envelope of delivery %d: %w", row.ID, err)
	}
	if err := q.SetWebhookDeliveryPayload(ctx, db.SetWebhookDeliveryPayloadParams{ID: row.ID, Payload: payload}); err != nil {
		return false, fmt.Errorf("storing the payload of delivery %d: %w", row.ID, err)
	}
	if err := n.enqueue(ctx, q, row.ID); err != nil {
		return false, err
	}
	return true, nil
}

// enqueue adds the deliver_webhook job and stores its id on the row.
func (n *Notifier) enqueue(ctx context.Context, q *db.Queries, id int64) error {
	args, err := json.Marshal(jobArgs{Delivery: id})
	if err != nil {
		return fmt.Errorf("marshalling the job of delivery %d: %w", id, err)
	}
	key := fmt.Sprintf("webhook:%d", id)
	if _, err := queue.EnqueueTx(ctx, q, queue.EnqueueParams{
		Kind:        JobKind,
		Args:        args,
		DedupeKey:   key,
		MaxAttempts: int(n.maxAttempts.Load()),
		CreatedBy:   "notify",
	}); err != nil {
		return fmt.Errorf("enqueueing delivery %d: %w", id, err)
	}
	job, err := q.GetActiveJobByDedupeKey(ctx, pgtype.Text{String: key, Valid: true})
	if err != nil {
		return fmt.Errorf("reading the job of delivery %d: %w", id, err)
	}
	if err := q.SetWebhookDeliveryJob(ctx, db.SetWebhookDeliveryJobParams{ID: id, JobID: pgtype.Int8{Int64: job.ID, Valid: true}}); err != nil {
		return fmt.Errorf("linking delivery %d to its job: %w", id, err)
	}
	return nil
}

// Emit is EmitTx in a transaction of its own, for callers with none.
func (n *Notifier) Emit(ctx context.Context, pool *pgxpool.Pool, ev Event) (int, error) {
	var created int
	err := inTx(ctx, pool, func(q *db.Queries) error {
		var err error
		created, err = n.EmitTx(ctx, q, ev)
		return err
	})
	return created, err
}

// Redeliver copies a delivery into a new row with a fresh id and enqueues it,
// so the log keeps both. The webhook must still be configured.
func (n *Notifier) Redeliver(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) (db.GetWebhookDeliveryRow, error) {
	var out db.GetWebhookDeliveryRow
	err := inTx(ctx, pool, func(q *db.Queries) error {
		old, err := q.GetWebhookDelivery(ctx, deliveryID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delivery %d: %w", deliveryID, ErrDeliveryNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading delivery %d: %w", deliveryID, err)
		}
		if _, ok := n.Webhook(old.Webhook); !ok {
			return fmt.Errorf("delivery %d names webhook %s: %w", deliveryID, old.Webhook, ErrUnknownWebhook)
		}
		var env Envelope
		if err := json.Unmarshal(old.Payload, &env); err != nil {
			return fmt.Errorf("reading the payload of delivery %d: %w", deliveryID, err)
		}
		// The key is unique per redelivery, so asking twice sends twice.
		key := fmt.Sprintf("redeliver:%d:%d", deliveryID, n.now().UnixNano())
		row, err := q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{
			Webhook: old.Webhook, Event: old.Event, SubjectDigest: old.SubjectDigest,
			Repository: old.Repository, Payload: []byte(`{}`), DedupeKey: key,
		})
		if err != nil {
			return fmt.Errorf("recording the redelivery of %d: %w", deliveryID, err)
		}
		env.ID = fmt.Sprint(row.ID)
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshalling the redelivery of %d: %w", deliveryID, err)
		}
		if err := q.SetWebhookDeliveryPayload(ctx, db.SetWebhookDeliveryPayloadParams{ID: row.ID, Payload: payload}); err != nil {
			return fmt.Errorf("storing the redelivery of %d: %w", deliveryID, err)
		}
		if err := n.enqueue(ctx, q, row.ID); err != nil {
			return err
		}
		out, err = q.GetWebhookDelivery(ctx, row.ID)
		return err
	})
	return out, err
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *db.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// jobArgs is the argument of a deliver_webhook job.
type jobArgs struct {
	Delivery int64 `json:"delivery"`
}

// orphanedError is what a swept delivery records.
const orphanedError = "the delivery job ended without an outcome"

// SweepOrphaned fails every pending delivery whose job failed or no longer
// exists, and that is more than a minute old. A worker that dies during the
// last attempt, or a job that fails before the handler can record it, leaves
// such a row; without the sweep it would read pending forever. It returns how
// many rows it failed.
func SweepOrphaned(ctx context.Context, q *db.Queries) (int64, error) {
	n, err := q.FailOrphanedWebhookDeliveries(ctx, orphanedError)
	if err != nil {
		return 0, fmt.Errorf("failing orphaned webhook deliveries: %w", err)
	}
	return n, nil
}
