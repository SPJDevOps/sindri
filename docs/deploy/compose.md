# Deploying with Docker Compose

<!-- commands checked by: scripts/deploy-compose-gate.sh -->

This runs Sindri and Postgres on one machine, with blobs on a local volume and generated
credentials. It is not the [quickstart](../quickstart.md): the quickstart signs in anyone
who opens a browser and has its password written in the file, so it must never be exposed.

## Files

Create a directory, and save this as `compose.yaml`:

<!-- compose:file -->
```yaml
name: sindri

services:
  postgres:
    image: postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
    restart: unless-stopped
    environment:
      POSTGRES_USER: sindri
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set it in .env}
      POSTGRES_DB: sindri
    volumes:
      - postgres:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U sindri -d sindri"]
      interval: 5s
      timeout: 5s
      retries: 30

  sindri:
    image: ghcr.io/spjdevops/sindri:${SINDRI_VERSION:?set it in .env}
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://sindri:${POSTGRES_PASSWORD}@postgres:5432/sindri?sslmode=disable
      SINDRI_TOKEN: ${SINDRI_TOKEN:?set it in .env}
    # Loopback only: a reverse proxy on this machine terminates TLS in front of it.
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./sindri.yaml:/etc/sindri/sindri.yaml:ro
      - blobs:/var/lib/sindri
    healthcheck:
      test: ["CMD", "/usr/local/bin/sindri", "probe", "http://127.0.0.1:8080/readyz"]
      interval: 10s
      timeout: 5s
      retries: 30

volumes:
  postgres:
  blobs:
```
<!-- /compose:file -->

Save this as `sindri.yaml`, with your own `publicUrl`:

<!-- compose:config -->
```yaml
listen: :8080
database: ${DATABASE_URL}
storage: /var/lib/sindri
# The address clients use. npm tarball links are built from it.
publicUrl: https://sindri.example.com
auth:
  # The bootstrap administrator token, for docker login and the API.
  static_token: ${SINDRI_TOKEN}
```
<!-- /compose:config -->

For people to sign in to the UI, add `auth.oidc` for your identity provider. The
[configuration reference](../configuration.md) lists every setting, including the S3
storage backend.

## Start it

This generates the credentials once, keeps them in `.env`, and starts both services:

<!-- compose:up -->
```bash
cat > .env <<EOF
SINDRI_VERSION=1.2.0
POSTGRES_PASSWORD=$(openssl rand -hex 24)
SINDRI_TOKEN=$(openssl rand -hex 32)
EOF
chmod 600 .env
docker compose up -d --wait
```
<!-- /compose:up -->

Keep `.env` safe. It holds the only copy of both secrets.

Next, put TLS in front of it with a [reverse proxy](reverse-proxy.md), and set up
[backups](../operations.md#backup-and-restore). To upgrade, follow
[Upgrading](../upgrading.md).
