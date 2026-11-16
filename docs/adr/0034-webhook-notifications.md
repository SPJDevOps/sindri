# 34. Webhook notifications on a new finding and on a promotion

- **Status:** Accepted
- **Date:** 2026-11-16
- **Relates to:** [ADR 20](0020-queue-table-and-background-worker.md),
  [ADR 31](0031-rescan-approved-subjects.md),
  [ADR 33](0033-open-source-boundary-and-cncf.md), roadmap Sprint 14 (SIN-117, SIN-112)

## Context

Since ADR 31 a re-scan can find a new CVE in an artifact already approved for
production, and nothing tells anyone: the finding shows in the API, the UI, and the
`sindri_approved_findings` gauge, which someone has to be looking at. A promotion is the
other moment people want to hear about, because deploy pipelines and change records
start from it. Both need to leave Sindri without a person polling.

## Decision

1. **Two events.**
   - `finding.new`: a Sindri scan attaches a non-empty `new_vulnerabilities` list (ADR 31)
     for a subject that has a current `approve` row in `lifecycle_current` at any stage.
     A finding on a subject nobody approved is not sent; it is not running anywhere.
     One event per attached scan evidence row, so a multi-platform image can send one
     per platform.
   - `lifecycle.promoted`: a promotion (`approve`) commits.

   Revoke, publish, and first-scan events are not sent. They can be added when adopters
   ask; adding an event is additive.
2. **Payload.** A JSON envelope:

   ```json
   {
     "id": "<delivery id>",
     "event": "finding.new",
     "occurredAt": "2026-11-16T10:00:00Z",
     "sindri": {"url": "https://sindri.example", "version": "1.6.0"},
     "subject": {"digest": "sha256:…", "repository": "docker", "stage": "prod"},
     "data": { … }
   }
   ```

   For `finding.new`, `subject.stage` is absent and `data` holds `newVulnerabilities`,
   `severityCounts`, `platform`, `approvedStages`, and `evidenceId`. For
   `lifecycle.promoted`, `data` holds `actor`, `reason`, `eventId` (the ledger row), and
   `evidenceIds`. A payload never carries a credential, a token, or artifact bytes.
3. **Signing.** Every delivery carries `X-Sindri-Event`, `X-Sindri-Delivery`,
   `X-Sindri-Timestamp` (Unix seconds), and
   `X-Sindri-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + body)>`.
   The timestamp is inside the signed string, so a receiver can refuse old replays.
4. **Delivery is a queue job.** Emitting an event inserts one row per matching webhook
   into `webhook_deliveries` and enqueues a `deliver_webhook` job, in the same
   transaction as the change that caused it: the promotion's ledger row, or the scan
   evidence row that carries `new_vulnerabilities`. Either both commit or neither does, so
   a failed emit fails the attach and the job retries with nothing lost and nothing
   doubled. Nothing is sent on the request path (ADR 20). A delivery's dedupe key is unique per webhook:
   `promoted:<ledger id>`, and for a finding `finding:<digest>:<platform>:<hash of the
   sorted new ids>`. A re-attached scan gets a new evidence id, so the key names the
   finding, not the row, and the same finding emitted twice sends once.
5. **Outcomes.** A 2xx response is delivered. 408, 429, 5xx, and network errors are
   retried with the queue's exponential backoff, up to `notifications.maxAttempts`
   (default 8). Any other response, or a webhook removed from the configuration, fails
   the delivery at once. Redirects are not followed. The delivery row records attempts,
   the last status, and the last error; errors and logs name the webhook and its host,
   never the path, query, headers, or body.
6. **Redelivery.** An administrator may redeliver a delivery through the API. It
   creates a new delivery row with the same payload and a fresh id, so the log keeps
   both.
7. **Configuration.** A server-level `notifications:` section with a list of webhooks,
   each with a `name`, `url`, `secret`, `events`, an optional `repositories` filter, and
   a `timeout`. Like `scanning:`, it has no manifest form. The URL must be https, or http
   on a loopback host, and must not carry userinfo. `config show`, the API, and the UI
   show only the scheme and host; the secret is never shown. The webhook set is reloaded
   on SIGHUP.
8. **Visibility.** Admin-only API routes list webhooks and deliveries and redeliver. An
   admin-only `/system` page shows the webhooks, a failing-webhook status line, and the
   delivery log. An artifact page shows administrators the deliveries for its subject.
   `sindri_webhook_deliveries_total{result}` counts attempts by outcome.
9. **Open.** Notifications ship in the open build and stay there (ADR 33). They are part
   of the security claim, not an add-on to it.

## Consequences

- A new CVE in something approved for production now reaches a person through whatever
  receiver they point Sindri at: a chat bridge, an incident tool, or a small service.
- A receiver must verify the signature and should check the timestamp. The docs carry
  examples.
- Deliveries are retained as rows; pruning them is a later question, like evidence
  retention.
- A webhook removed from the configuration leaves its queued deliveries to fail, which
  the log shows, rather than vanishing.

## Alternatives rejected

- **Webhooks per repository, declared in manifests.** Teams could own them, but the
  targets and secrets are an operator's concern, and a reconciled kind doubles the
  golden tests for little gain while the event set is two.
- **Sending inline on the request path.** A slow receiver would slow promotions, and a
  crash between commit and send would lose the event. The queue table already solves
  both.
- **Blocking private-network targets.** Most receivers are internal. Only an
  administrator can configure a target, so an SSRF guard would block the common case and
  protect against nobody.
- **Notifying on every scan with new findings.** Unapproved subjects are not running,
  and every re-publish would page someone.
- **Unsigned payloads, or a signature over the body alone.** Without a signature a
  receiver cannot tell Sindri from anyone else; without the timestamp a captured delivery
  can be replayed forever.
- **Slack, email, or per-user subscriptions.** Each is a formatting or identity problem
  of its own. A webhook is the primitive they can all be built on.
