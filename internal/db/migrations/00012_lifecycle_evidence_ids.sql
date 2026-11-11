-- +goose Up
-- +goose StatementBegin

-- The evidence each gate read when an approve was recorded (ADR 29, ADR 30).
-- Nullable and additive: a revoke, an ungated approve, and every event written
-- before this migration have NULL, and a binary one version back never names
-- the column. The append-only trigger on lifecycle_events guards UPDATE and
-- DELETE, not the schema, so adding a column is not blocked by it.
ALTER TABLE lifecycle_events ADD COLUMN evidence_ids uuid[] NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE lifecycle_events DROP COLUMN IF EXISTS evidence_ids;

-- +goose StatementEnd
