-- +goose Up
-- +goose StatementBegin

-- One row per webhook per event (ADR 34): the payload that was or will be
-- sent, and what happened when it was. The delivery itself is a deliver_webhook
-- job in the queue; this row is the log an operator reads, so it outlives the
-- job, which retention deletes. job_id is therefore a plain column, not a
-- foreign key. webhook is the configured name, not a foreign key either: a
-- webhook removed from the configuration leaves its history behind.
--
-- dedupe_key makes emission idempotent per webhook, so a retried scan job or
-- a repeated promotion commit cannot send twice. Additive: a binary one
-- version back never names the table.
CREATE TABLE webhook_deliveries (
    id             bigserial   PRIMARY KEY,
    webhook        text        NOT NULL,
    event          text        NOT NULL,
    subject_digest text        NOT NULL,
    repository     text        NOT NULL,
    payload        jsonb       NOT NULL,
    dedupe_key     text        NOT NULL,
    job_id         bigint,
    state          text        NOT NULL DEFAULT 'pending'
                               CHECK (state IN ('pending', 'delivered', 'failed')),
    attempts       integer     NOT NULL DEFAULT 0,
    last_status    integer,
    last_error     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    delivered_at   timestamptz,
    finished_at    timestamptz,
    CONSTRAINT webhook_deliveries_dedupe_unique UNIQUE (webhook, dedupe_key)
);

CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook, created_at DESC, id DESC);
CREATE INDEX webhook_deliveries_state_idx ON webhook_deliveries (state, created_at DESC);
CREATE INDEX webhook_deliveries_subject_idx ON webhook_deliveries (subject_digest, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS webhook_deliveries;

-- +goose StatementEnd
