-- +goose Up
-- +goose StatementBegin

-- Names a hosted repository has published and then deleted (SIN-52). A
-- published version is immutable, so deleting its file must not free the
-- number: a range install could otherwise pick up new bytes under the old one.
-- The row lives and dies with its package: a hosted package row is the name's
-- claim and outlives its files, and deleting the repository cascades here.
-- npm checks by version and PyPI by filename within the package, so the table
-- holds both. Rows deleted before this migration are not remembered.
CREATE TABLE retired_files (
    package_id uuid        NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    version    text        NOT NULL,
    filename   text        NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (package_id, version, filename)
);

-- The PyPI check looks a filename up across versions.
CREATE INDEX retired_files_filename_idx ON retired_files (package_id, filename);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS retired_files;

-- +goose StatementEnd
