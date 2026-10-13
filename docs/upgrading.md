# Upgrading Sindri

An upgrade is replacing one binary or one image. There is no separate migrate
step and no upgrade tool.

## What happens on startup

`sindri serve` applies every outstanding database migration before it accepts a
request. The migrations are embedded in the binary, so the schema a binary
needs always ships with it. They run under a Postgres advisory lock: when
several instances start at once, one migrates and the others wait, so a rolling
restart is safe.

The blob store needs nothing. Blobs are content-addressed bytes whose layout
does not change between releases.

## Upgrading

1. Back up Postgres. Migrations are written to be safe, but a backup is what
   makes any mistake recoverable.
2. Replace the binary, or change the image tag:

   ```bash
   # Compose quickstart
   SINDRI_IMAGE=ghcr.io/spjdevops/sindri:<version> docker compose up -d

   # Binary
   tar -xzf sindri_<version>_linux_amd64.tar.gz sindri
   install -m 0755 sindri /usr/local/bin/sindri
   systemctl restart sindri
   ```

3. Check the log for `database schema is up to date`. Its `applied` field
   counts the migrations this start ran.

Upgrade one release at a time. Each release's migrations are only guaranteed
to be compatible with the release before it.

## Rolling back

Every migration stays backward compatible for one release, so the previous
binary runs against the newer schema. To roll back one release, start the
previous binary or image. It finds no migrations to apply, leaves the newer
schema in place, and serves. A rollback does not need a database restore.

Rolling back further than one release is not supported without restoring the
backup taken before the upgrade.

## Pinning

The quickstart uses `ghcr.io/spjdevops/sindri:latest` so that it works first
time. For anything you depend on, pin a version tag, or a digest, and upgrade
on purpose.
