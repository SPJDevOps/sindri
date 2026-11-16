# Notifications

<!-- commands checked by: TestDocs_NotificationsExamplesHold -->

Sindri can tell another system when something happens that a person should know about.
It POSTs a signed JSON document, a webhook, to each URL you configure. Point it at a chat
bridge, an incident tool, or a small service of your own.

Two events are sent:

| Event | Sent when |
|---|---|
| `finding.new` | A scan finds a vulnerability that the previous scan of the same artifact did not have, and the artifact is approved at some stage. |
| `lifecycle.promoted` | An artifact is promoted (approved) to a stage. |

`finding.new` is the one that matters most. A re-scan can find a new CVE in something that
is already running in production, and without a webhook nobody hears about it until they
look. A finding on an artifact that nobody approved is not sent: it is not running anywhere.
See [Re-scanning](scanning.md#re-scanning) for how re-scans run and what counts as new.

The design is recorded in [ADR 34](adr/0034-webhook-notifications.md).

## Configure a webhook

Webhooks are a server setting, in the `notifications:` section of the configuration file:

```yaml
notifications:
  webhooks:
    - name: security-team
      url: https://hooks.example.com/sindri
      secret: ${SINDRI_WEBHOOK_SECRET}
      events: [finding.new, lifecycle.promoted]
      repositories: [docker, pypi]   # optional; leave it out for every repository
      timeout: 10s
  maxAttempts: 8
```

- **`secret`** signs every delivery. It must be at least 32 bytes. Keep it out of the file
  with `${VAR}`; `openssl rand -hex 32` makes a good one.
- **`url`** must be https, or http on a loopback address. It may not contain a user name or
  password. Sindri never prints more of it than the scheme and host, because webhook URLs
  often carry a token in the path.
- **`repositories`** filters by the repository the event happened in: the repository a
  promotion was made in, or the repository the scan read the artifact from.

The full list of keys is in the [configuration reference](configuration.md#notifications).
`kill -HUP` reloads the webhook list with the rest of the file. A webhook you remove stops
receiving: its queued deliveries are recorded as failed rather than sent.

## What a delivery looks like

Each delivery is an HTTP `POST` with a JSON body and these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `sindri/<version>` |
| `X-Sindri-Event` | `finding.new` or `lifecycle.promoted` |
| `X-Sindri-Delivery` | The delivery id, the same as the body's `id` |
| `X-Sindri-Timestamp` | When this attempt was sent, in Unix seconds |
| `X-Sindri-Signature` | `sha256=` and the hex HMAC-SHA256 of the timestamp, a `.`, and the body |

A `finding.new` body:

```json
{
  "id": "41",
  "event": "finding.new",
  "occurredAt": "2026-11-16T10:00:00Z",
  "sindri": {"url": "https://sindri.example.com", "version": "1.6.0"},
  "subject": {"digest": "sha256:9f86d0…", "repository": "docker"},
  "data": {
    "newVulnerabilities": ["CVE-2026-1234"],
    "severityCounts": {"CRITICAL": 1, "HIGH": 3, "MEDIUM": 0, "LOW": 2, "UNKNOWN": 0},
    "platform": "linux/amd64",
    "approvedStages": ["prod"],
    "evidenceId": "0b6f…"
  }
}
```

`severityCounts` counts every finding in the new scan, not only the new ones. A
multi-platform image sends one delivery per platform that has something new.

A `lifecycle.promoted` body:

```json
{
  "id": "42",
  "event": "lifecycle.promoted",
  "occurredAt": "2026-11-16T10:05:00Z",
  "sindri": {"url": "https://sindri.example.com", "version": "1.6.0"},
  "subject": {"digest": "sha256:9f86d0…", "repository": "docker", "stage": "prod"},
  "data": {
    "actor": "alice@example.com",
    "reason": "release 4.2",
    "eventId": 17,
    "evidenceIds": ["0b6f…"]
  }
}
```

A body never carries a credential, a token, or the artifact itself. Look the artifact up
by its digest in the UI or the API.

## Verify the signature

Check the signature before you trust a delivery, and refuse one whose timestamp is more
than a few minutes old so that a captured delivery cannot be replayed. Compute the HMAC
over the raw body bytes exactly as received, not over JSON you have parsed and
re-encoded.

In a shell, with the body saved to `body.json`:

```bash
expected="sha256=$(printf '%s.%s' "$TIMESTAMP" "$(cat body.json)" \
  | openssl dgst -sha256 -hmac "$SINDRI_WEBHOOK_SECRET" -hex | sed 's/^.* //')"
[ "$expected" = "$SIGNATURE" ] && echo valid
```

In Go:

```go
func verify(secret []byte, r *http.Request, body []byte) bool {
	ts := r.Header.Get("X-Sindri-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-Sindri-Signature")))
}
```

## Retries and failures

A delivery is a queue job, so a slow or broken receiver never slows Sindri down, and a
restart loses nothing.

- **A 2xx response** means delivered.
- **408, 429, a 5xx, a timeout, or a network error** is retried with exponential backoff,
  up to `maxAttempts` attempts in all. After the last one the delivery is recorded as
  failed.
- **Any other response**, including a redirect, fails the delivery at once. Sindri does
  not follow redirects; point the webhook at the final URL.

The same event is never delivered twice to one webhook, even if the job that caused it is
retried. Receivers should still treat `X-Sindri-Delivery` as an idempotency key, because a
receiver that answers too slowly may be sent the same delivery again.

## See what was sent

Administrators can see every webhook and its deliveries:

- **In the UI**, the **System** page shows each webhook with its target host and the
  number of failed deliveries. The line at the top says whether any webhook is failing,
  which means its newest finished delivery failed, so a receiver that recovers clears it.
  It links to the delivery log: when each delivery was made, the response status, the
  attempts, the next retry, and the error. An artifact's page lists the notifications
  sent about it.
- **In the API**, these routes need an administrator:

  | Route | Answers |
  |---|---|
  | `GET /api/v1/notifications/webhooks` | Each webhook: name, events, repositories, target host, failed deliveries, and whether it is failing |
  | `GET /api/v1/notifications/deliveries?webhook=&state=&digest=&limit=` | The delivery log, newest first; `state` is `pending`, `delivered`, or `failed` |
  | `GET /api/v1/notifications/deliveries/{id}` | One delivery, with the body that was sent |
  | `POST /api/v1/notifications/deliveries/{id}/redeliver` | 202, and a new delivery with the same body and a new id |

  Redelivering after you fix a receiver:

  ```bash
  curl -fsS -X POST -H "Authorization: Bearer $TOKEN" \
    "$SINDRI_URL/api/v1/notifications/deliveries/41/redeliver"
  ```

  A webhook that is no longer configured answers 409 `WEBHOOK_NOT_CONFIGURED`.

Two metrics count deliveries; see [Operating Sindri](operations.md#metrics-and-alerts):
`sindri_webhook_deliveries_total{result}` counts attempts that were `delivered`,
`retried`, or `failed`, and `sindri_webhook_delivery_duration_seconds` times them.
