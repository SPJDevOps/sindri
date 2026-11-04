-- +goose Up
-- +goose StatementBegin

-- When a refresh of a proxy's package document first showed that upstream no
-- longer lists a placed file (SIN-54). A flagged file is not served and not
-- placed; retention removes the row once it has been flagged longer than the
-- stale allowance, and a later refresh that lists the file again clears it.
-- Only proxy rows are ever flagged: hosted files are the repository's own.
ALTER TABLE files ADD COLUMN upstream_gone_at timestamptz NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE files DROP COLUMN IF EXISTS upstream_gone_at;

-- +goose StatementEnd
