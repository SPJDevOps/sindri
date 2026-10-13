# Quickstart

A registry with a web UI on one machine, in about two minutes, with nothing
installed but Docker. You push an image, sign in, and look at it.

This setup is for trying Sindri out. Its identity provider signs in anyone who
opens a browser on this machine, as an administrator. For a real deployment,
see [Going further](#going-further).

## 1. Save the Compose file

Save this as `compose.yaml` in an empty directory. It is the same file as
[`deploy/quickstart/compose.yaml`](../deploy/quickstart/compose.yaml), so
`curl -fsSLO https://raw.githubusercontent.com/SPJDevOps/sindri/main/deploy/quickstart/compose.yaml`
fetches it too.

It runs three containers:

- **Postgres** for metadata.
- **Dex**, a development identity provider whose mock connector signs everyone
  in as one example user.
- **Sindri** itself, storing blobs in a named volume.

<!-- quickstart:compose -->
```yaml
# Sindri quickstart: a registry with a web UI on one machine, nothing to
# install but Docker. docs/quickstart.md walks through it.
#
#   export SINDRI_TOKEN=$(openssl rand -hex 16)
#   docker compose up -d --wait
#
# DEVELOPMENT ONLY. Dex here signs in anyone who opens the browser as an
# example user in the `authors` group, and `authors` administer this server.
# The database password is in this file. Never deploy this as it is: point
# auth.oidc at your identity provider and the database at a managed Postgres.
name: sindri-quickstart

services:
  postgres:
    image: postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
    environment:
      POSTGRES_USER: sindri
      POSTGRES_PASSWORD: sindri
      POSTGRES_DB: sindri
    volumes:
      - postgres:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U sindri -d sindri"]
      interval: 2s
      timeout: 5s
      retries: 30

  # The identity provider for the UI. Its mock connector signs everyone in as
  # one example user, with no password, which is what makes this a demo.
  dex:
    image: ghcr.io/dexidp/dex:v2.46.0@sha256:933fcd3f523338c847b88ef84a7145fb2fcb7b9917f08418612afeaaa2001777
    command: ["dex", "serve", "/etc/dex/config.yaml"]
    configs:
      - source: dex
        target: /etc/dex/config.yaml
    # Sindri shares this network namespace, so it reaches Dex at the same
    # http://localhost:5556 the browser uses, and the token issuer matches.
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:5556:5556"

  sindri:
    image: ${SINDRI_IMAGE:-ghcr.io/spjdevops/sindri:latest}
    network_mode: "service:dex"
    depends_on:
      postgres:
        condition: service_healthy
      dex:
        condition: service_started
    environment:
      SINDRI_TOKEN: ${SINDRI_TOKEN:?export SINDRI_TOKEN first, for example SINDRI_TOKEN=$(openssl rand -hex 16)}
    configs:
      - source: sindri
        target: /etc/sindri/sindri.yaml
    volumes:
      - blobs:/var/lib/sindri
    healthcheck:
      test: ["CMD", "/usr/local/bin/sindri", "probe", "http://127.0.0.1:8080/readyz"]
      interval: 2s
      timeout: 5s
      retries: 30

configs:
  sindri:
    content: |
      listen: :8080
      storage: /var/lib/sindri
      database: postgres://sindri:sindri@postgres:5432/sindri?sslmode=disable
      auth:
        # The bootstrap administrator token: docker login uses it below.
        static_token: $${SINDRI_TOKEN}
        admins: [authors]
        oidc:
          issuer: http://localhost:5556/dex
          clientId: sindri
          clientSecret: quickstart-not-a-secret
          redirectUrl: http://localhost:8080/auth/callback
          scopes: [openid, profile, email, groups]
  dex:
    content: |
      issuer: http://localhost:5556/dex
      storage:
        type: memory
      web:
        http: 0.0.0.0:5556
      oauth2:
        skipApprovalScreen: true
      staticClients:
        - id: sindri
          name: Sindri
          secret: quickstart-not-a-secret
          redirectURIs: ["http://localhost:8080/auth/callback"]
      connectors:
        - type: mockCallback
          id: mock
          name: Example user

volumes:
  postgres:
  blobs:
```
<!-- /quickstart:compose -->

## 2. Start it, push an image, open the UI

<!-- quickstart:begin -->
```bash
export SINDRI_TOKEN=$(openssl rand -hex 16)
docker compose up -d --wait

echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin
docker pull alpine:3.20
docker tag alpine:3.20 localhost:8080/demo/app:1.0
docker push localhost:8080/demo/app:1.0

open http://localhost:8080/ui/
```
<!-- quickstart:end -->

`SINDRI_TOKEN` is the bootstrap administrator credential: `docker login` uses
it as the password. On Linux, use `xdg-open` instead of `open`, or paste the
URL into a browser.

The browser goes through Dex and back, and lands on the repository list
signed in as the example user. Open `demo/app`, then tag `1.0`, to see the
artifact page: digest, owner, provenance, the manifest and its layers, and its
state in each lifecycle stage.

## 3. Stop it

```bash
docker compose down        # keeps the data volumes
docker compose down -v     # deletes them too
```

## Going further

- **Configuration as code.** Repositories, access, and lifecycle stages
  belong in `sindri.yaml`, reconciled on every start. See
  [ARCHITECTURE.md section 8](../ARCHITECTURE.md#8-configuration-as-code).
- **Your identity provider.** Point `auth.oidc` at it and set `auth.admins`
  to a group of yours. Then people use their ID token as the password for
  docker, pip, and twine. See
  [ADR 18](adr/0018-authentication-and-rbac-in-v1.md).
- **Storage.** `storage: s3://bucket/prefix` uses any S3-compatible store.
- **Garbage collection.** `sindri gc --dry-run` lists what nothing
  references any more. See
  [ADR 19](adr/0019-garbage-collection-roots-write-intent-and-sweep.md).
- **Upgrading** is replacing the image. Migrations apply on startup and stay
  compatible with the previous release. See [upgrading](upgrading.md).
