package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
)

// Delivery list limits: the default, and the most one request may ask for.
const (
	defaultDeliveryLimit = 50
	maxDeliveryLimit     = 500
)

// webhookResponse describes one configured webhook. Target is the scheme and
// host only: the URL's path and query are the secret part of many receivers,
// and the signing secret is never returned (ADR 34).
type webhookResponse struct {
	Name             string   `json:"name"`
	Events           []string `json:"events"`
	Repositories     []string `json:"repositories"`
	Target           string   `json:"target"`
	FailedDeliveries int64    `json:"failed_deliveries"`
}

// deliveryResponse is one delivery. Payload is set on the single GET only.
type deliveryResponse struct {
	ID          int64   `json:"id"`
	Webhook     string  `json:"webhook"`
	Event       string  `json:"event"`
	Digest      string  `json:"digest"`
	Repository  string  `json:"repository"`
	State       string  `json:"state"`
	Attempts    int32   `json:"attempts"`
	LastStatus  *int32  `json:"last_status"`
	LastError   *string `json:"last_error"`
	CreatedAt   string  `json:"created_at"`
	DeliveredAt *string `json:"delivered_at"`
	FinishedAt  *string `json:"finished_at"`
	// NextRetryAt is set only while the delivery is pending.
	NextRetryAt *string `json:"next_retry_at"`
}

// deliveryDetail adds the exact JSON body sent, which lists omit.
type deliveryDetail struct {
	deliveryResponse
	Payload rawJSON `json:"payload"`
}

// rawJSON marshals as the bytes it holds.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

func optText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func optTime(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := timestamp(t.Time)
	return &s
}

func newDelivery(id int64, webhook, event, digest, repository, state string, attempts int32, lastStatus pgtype.Int4,
	lastError pgtype.Text, createdAt pgtype.Timestamptz, delivered, finished, runAfter pgtype.Timestamptz) deliveryResponse {
	d := deliveryResponse{
		ID: id, Webhook: webhook, Event: event, Digest: digest, Repository: repository,
		State: state, Attempts: attempts,
		LastError: optText(lastError), CreatedAt: *optTime(createdAt),
		DeliveredAt: optTime(delivered), FinishedAt: optTime(finished),
	}
	if lastStatus.Valid {
		d.LastStatus = &lastStatus.Int32
	}
	if state == "pending" {
		d.NextRetryAt = optTime(runAfter)
	}
	return d
}

func detailJSON(row db.GetWebhookDeliveryRow) deliveryDetail {
	return deliveryDetail{
		deliveryResponse: newDelivery(row.ID, row.Webhook, row.Event, row.SubjectDigest, row.Repository, row.State,
			row.Attempts, row.LastStatus, row.LastError,
			pgtype.Timestamptz{Time: row.CreatedAt, Valid: true}, row.DeliveredAt, row.FinishedAt, row.JobRunAfter),
		Payload: row.Payload,
	}
}

func (a *api) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "list notification webhooks") {
		return
	}
	failed, err := a.notifyDB.Queries().CountFailedWebhookDeliveriesByWebhook(r.Context())
	if err != nil {
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	counts := make(map[string]int64, len(failed))
	for _, f := range failed {
		counts[f.Webhook] = f.Failed
	}
	hooks := a.notifier.Webhooks()
	out := make([]webhookResponse, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, webhookResponse{
			Name:             h.Name,
			Events:           nonNil(h.Events),
			Repositories:     nonNil(h.Repositories),
			Target:           model.URLHost(h.URL),
			FailedDeliveries: counts[h.Name],
		})
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[webhookResponse]{Items: out})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *api) listDeliveries(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "list notification deliveries") {
		return
	}
	q := r.URL.Query()
	params := db.ListWebhookDeliveriesParams{RowLimit: defaultDeliveryLimit}
	if v := q.Get("webhook"); v != "" {
		params.Webhook = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("state"); v != "" {
		switch v {
		case "pending", "delivered", "failed":
		default:
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "state must be pending, delivered, or failed")
			return
		}
		params.State = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("digest"); v != "" {
		d, err := model.ParseDigest(v)
		if err != nil {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "digest is not a valid digest")
			return
		}
		params.SubjectDigest = pgtype.Text{String: d.String(), Valid: true}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > maxDeliveryLimit {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "limit must be an integer from 1 to "+strconv.Itoa(maxDeliveryLimit))
			return
		}
		params.RowLimit = int32(n)
	}
	rows, err := a.notifyDB.Queries().ListWebhookDeliveries(r.Context(), params)
	if err != nil {
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	out := make([]deliveryResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, newDelivery(row.ID, row.Webhook, row.Event, row.SubjectDigest, row.Repository, row.State,
			row.Attempts, row.LastStatus, row.LastError,
			pgtype.Timestamptz{Time: row.CreatedAt, Valid: true}, row.DeliveredAt, row.FinishedAt, row.JobRunAfter))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[deliveryResponse]{Items: out})
}

func deliveryID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func (a *api) getDelivery(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "read notification deliveries") {
		return
	}
	id, ok := deliveryID(r)
	if !ok {
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "delivery not found")
		return
	}
	row, err := a.notifyDB.Queries().GetWebhookDelivery(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "delivery not found")
		return
	}
	if err != nil {
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	a.writeJSON(w, r, http.StatusOK, detailJSON(row))
}

func (a *api) redeliver(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "redeliver notifications") {
		return
	}
	id, ok := deliveryID(r)
	if !ok {
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "delivery not found")
		return
	}
	row, err := a.notifier.Redeliver(r.Context(), a.notifyDB.Pool(), id)
	switch {
	case errors.Is(err, notify.ErrDeliveryNotFound):
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "delivery not found")
	case errors.Is(err, notify.ErrUnknownWebhook):
		a.writeError(w, r, http.StatusConflict, "WEBHOOK_NOT_CONFIGURED", "the delivery's webhook is no longer configured")
	case err != nil:
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
	default:
		a.writeJSON(w, r, http.StatusAccepted, detailJSON(row))
	}
}
