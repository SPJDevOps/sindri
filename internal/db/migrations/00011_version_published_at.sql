-- +goose Up
-- +goose StatementBegin

-- When a version was published (SIN-60). npm clients read it from the
-- packument's time map; yarn 4 refuses a version it cannot date. The column is
-- added bare and the default set after, so rows that exist now stay NULL: their
-- real time is not known and Sindri does not invent one. A NULL row is left out
-- of the time map. New rows take the insert time from the default, so no write
-- path needs to change, and an older binary that never names the column still
-- works against this schema.
ALTER TABLE versions ADD COLUMN published_at timestamptz NULL;
ALTER TABLE versions ALTER COLUMN published_at SET DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE versions DROP COLUMN IF EXISTS published_at;

-- +goose StatementEnd
