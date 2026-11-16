package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/version"
	"github.com/spj/sindri/internal/worker"
)

// maxResponseBytes is how much of a receiver's response is read, and
// discarded, so a hostile or broken receiver cannot make a delivery slow or
// large.
const maxResponseBytes = 4 << 10

// Sign returns the X-Sindri-Signature value for a delivery: `sha256=` and the
// hex HMAC-SHA256 of `<timestamp>.<body>` under the secret. The timestamp
// (Unix seconds) is inside the signed string so a receiver can refuse replays.
func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Outcome is what an attempt's result means for the delivery.
type Outcome int

const (
	// Delivered means the receiver answered 2xx.
	Delivered Outcome = iota
	// Retry means a transient failure; the queue tries again until attempts run out.
	Retry
	// Final means no retry can help, such as a 404 or a redirect.
	Final
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case Retry:
		return "retry"
	default:
		return "final"
	}
}

// Decide maps one attempt's HTTP status, or its transport error, to an
// Outcome (ADR 34). 408, 429, 5xx, and network errors are retried; any other
// non-2xx response, including a redirect, is final.
func Decide(status int, err error) Outcome {
	switch {
	case err != nil:
		return Retry
	case status >= 200 && status < 300:
		return Delivered
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return Retry
	default:
		return Final
	}
}

// DelivererOptions configures a Deliverer.
type DelivererOptions struct {
	Notifier *Notifier
	DB       *db.DB
	// Client defaults to one with no redirect following. Whatever is passed
	// has its CheckRedirect replaced: a redirect is never followed.
	Client *http.Client
	// Now defaults to time.Now, and stamps X-Sindri-Timestamp.
	Now    func() time.Time
	Logger *slog.Logger
}

// Deliverer is the handler of deliver_webhook jobs.
type Deliverer struct {
	notifier *Notifier
	queries  *db.Queries
	client   *http.Client
	now      func() time.Time
	log      *slog.Logger
}

// NewDeliverer returns a Deliverer.
func NewDeliverer(opts DelivererOptions) *Deliverer {
	client := &http.Client{}
	if opts.Client != nil {
		c := *opts.Client
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	d := &Deliverer{notifier: opts.Notifier, queries: opts.DB.Queries(), client: client, now: opts.Now, log: opts.Logger}
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d
}

// Handle delivers one webhook. It has the shape of worker.Handler.
//
// Its errors name the webhook and the receiver's host, never the path, query,
// headers, or body of the request or response (security rule 4).
func (d *Deliverer) Handle(ctx context.Context, job queue.Job) (json.RawMessage, error) {
	var args jobArgs
	if err := json.Unmarshal(job.Args, &args); err != nil || args.Delivery == 0 {
		return nil, worker.Permanent(errors.New("deliver_webhook job has no delivery id"))
	}
	row, err := d.queries.GetWebhookDelivery(ctx, args.Delivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, worker.Permanent(fmt.Errorf("delivery %d does not exist", args.Delivery))
	}
	if err != nil {
		return nil, fmt.Errorf("reading delivery %d: %w", args.Delivery, err)
	}
	switch row.State {
	case "delivered":
		// A retry after the result was lost: nothing to send.
		return statusResult(int(row.LastStatus.Int32)), nil
	case "failed":
		return nil, worker.Permanent(fmt.Errorf("delivery %d already failed", row.ID))
	}

	hook, ok := d.notifier.Webhook(row.Webhook)
	if !ok {
		err := fmt.Errorf("webhook %s is no longer configured", row.Webhook)
		if merr := d.queries.MarkWebhookFailed(ctx, db.MarkWebhookFailedParams{ID: row.ID, LastError: err.Error()}); merr != nil {
			return nil, fmt.Errorf("recording the failure of delivery %d: %w", row.ID, merr)
		}
		return nil, worker.Permanent(err)
	}

	status, reqErr := d.send(ctx, hook, row)
	if ctx.Err() != nil && reqErr != nil {
		// The worker is stopping or lost its lease; this attempt says
		// nothing about the receiver.
		return nil, ctx.Err()
	}

	outcome := Decide(status, reqErr)
	var failure error
	switch {
	case reqErr != nil:
		failure = reqErr
	case outcome != Delivered:
		failure = fmt.Errorf("webhook %s (%s): receiver answered %d", hook.Name, hostOf(hook.URL), status)
	}

	attempt := db.RecordWebhookAttemptParams{ID: row.ID}
	if status != 0 {
		attempt.LastStatus = pgtype.Int4{Int32: int32(status), Valid: true} //nolint:gosec // an HTTP status
	}
	if failure != nil {
		attempt.LastError = pgtype.Text{String: failure.Error(), Valid: true}
	}
	if err := d.queries.RecordWebhookAttempt(ctx, attempt); err != nil {
		return nil, fmt.Errorf("recording an attempt of delivery %d: %w", row.ID, err)
	}

	switch outcome {
	case Delivered:
		if err := d.queries.MarkWebhookDelivered(ctx, row.ID); err != nil {
			return nil, fmt.Errorf("recording delivery %d: %w", row.ID, err)
		}
		return statusResult(status), nil
	case Final:
		if err := d.queries.MarkWebhookFailed(ctx, db.MarkWebhookFailedParams{ID: row.ID, LastError: failure.Error()}); err != nil {
			return nil, fmt.Errorf("recording the failure of delivery %d: %w", row.ID, err)
		}
		return nil, worker.Permanent(failure)
	default:
		// FailJob requeues unless attempts have run out, and counts the
		// attempt when the job is claimed, so this is the last one when
		// Attempts has reached MaxAttempts.
		if job.Attempts >= job.MaxAttempts {
			if err := d.queries.MarkWebhookFailed(ctx, db.MarkWebhookFailedParams{ID: row.ID, LastError: failure.Error()}); err != nil {
				return nil, fmt.Errorf("recording the failure of delivery %d: %w", row.ID, err)
			}
		}
		return nil, failure
	}
}

// send POSTs the payload once. It returns the status, or an error already
// reduced to the webhook name and host.
func (d *Deliverer) send(ctx context.Context, hook Webhook, row db.GetWebhookDeliveryRow) (int, error) {
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(row.Payload))
	if err != nil {
		// The URL is not in the error: it is a secret.
		return 0, fmt.Errorf("webhook %s (%s): building the request failed", hook.Name, hostOf(hook.URL))
	}
	ts := d.now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sindri/"+version.Version)
	req.Header.Set("X-Sindri-Event", row.Event)
	req.Header.Set("X-Sindri-Delivery", strconv.FormatInt(row.ID, 10))
	req.Header.Set("X-Sindri-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Sindri-Signature", Sign(hook.Secret, ts, row.Payload))

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, describeRequestError(ctx, hook, timeout, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, nil
}

// describeRequestError reduces a transport error to something safe to store
// and log. net/http's *url.Error quotes the whole URL, so only its inner
// error, which names addresses and no path, is kept.
func describeRequestError(ctx context.Context, hook Webhook, timeout time.Duration, err error) error {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("webhook %s (%s): no answer within %s", hook.Name, hostOf(hook.URL), timeout)
	}
	return fmt.Errorf("webhook %s (%s): %w", hook.Name, hostOf(hook.URL), cause)
}

// hostOf is the host of a webhook URL, with its port.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "unknown host"
	}
	return u.Host
}

func statusResult(status int) json.RawMessage {
	return json.RawMessage(`{"status":` + strconv.Itoa(status) + `}`)
}
