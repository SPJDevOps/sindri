# Deploying on Kubernetes with Helm

<!-- commands checked by: scripts/helm-gate.sh -->

The chart runs Sindri as **one replica**. Upload sessions live in process memory, so a
chunked `docker push` would break if two pods shared the work. The chart has no replica
setting for that reason, and a restart or upgrade has brief downtime
([ADR 28](../adr/0028-helm-chart-location-and-publishing.md)).

## What you need first

The chart deploys Sindri and nothing else. Bring:

- Kubernetes, and Helm 3.8 or later.
- A Postgres database. Sindri creates its schema on first start.
- An S3-compatible bucket, or a volume for the filesystem backend (see [Storage](#storage)).
- For people to sign in to the UI, an OIDC identity provider. The
  [configuration reference](../configuration.md) describes `auth.oidc`.

## Install

The `config` value **is** `sindri.yaml`. The chart writes it to a ConfigMap unchanged, so
every key in the [configuration reference](../configuration.md) works here. Secrets stay
out of it: the file names them with `${VAR}`, and they reach the pod from a Secret.

Save this as `values.yaml`, with your own `publicUrl`, bucket, and any other settings:

<!-- helm:values -->
```yaml
config:
  database: ${DATABASE_URL}
  storage:
    backend: s3
    bucket: sindri-blobs
    endpoint: ${S3_ENDPOINT}
    pathStyle: true
    accessKeyId: ${S3_ACCESS_KEY_ID}
    secretAccessKey: ${S3_SECRET_ACCESS_KEY}
  # The address clients use. npm tarball links are built from it.
  publicUrl: https://sindri.example.com
  auth:
    # The bootstrap administrator token, for docker login and the API.
    static_token: ${SINDRI_TOKEN}
envFrom:
  - secretRef:
      name: sindri-env
```
<!-- /helm:values -->

On AWS, leave out `endpoint`, `pathStyle`, and both keys to use the pod's ambient
credentials.

Export the five values, generating the token with `openssl rand -hex 32`. Then create the
Secret and install:

<!-- helm:install -->
```bash
kubectl create namespace sindri
kubectl -n sindri create secret generic sindri-env \
  --from-literal=DATABASE_URL="$DATABASE_URL" \
  --from-literal=S3_ENDPOINT="$S3_ENDPOINT" \
  --from-literal=S3_ACCESS_KEY_ID="$S3_ACCESS_KEY_ID" \
  --from-literal=S3_SECRET_ACCESS_KEY="$S3_SECRET_ACCESS_KEY" \
  --from-literal=SINDRI_TOKEN="$SINDRI_TOKEN"
helm install sindri oci://ghcr.io/spjdevops/charts/sindri --version 1.2.0 \
  --namespace sindri --values values.yaml --wait --timeout 5m
```
<!-- /helm:install -->

The chart refuses to render without `database`, `storage`, and `auth.static_token`,
because Sindri's own defaults for them are for local development.

## Reaching it

Sindri needs its own hostname, because Docker requires `/v2` at the root of the host, so it
cannot be served under a path. TLS ends at the Ingress and Sindri receives plain HTTP,
which needs no forwarded headers. `publicUrl` must be the https address clients use.

> **ingress-nginx is retired.** The `kubernetes/ingress-nginx` project is archived, so
> expect no more releases or security fixes. The example below is tested against its last
> release, controller v1.15.1, because many clusters still run it. On a new cluster, choose
> another controller and apply the settings listed after the example.

Save this as `ingress.yaml`:

<!-- helm:ingress -->
```yaml
ingress:
  enabled: true
  className: nginx
  host: sindri.example.com
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "900"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "900"
  tls:
    - secretName: sindri-tls
      hosts: [sindri.example.com]
```
<!-- /helm:ingress -->

Create the certificate's Secret, or let cert-manager create it, then add
`--values ingress.yaml` to your `helm install` or `helm upgrade` command:

<!-- helm:tls -->
```bash
kubectl -n sindri create secret tls sindri-tls --cert=fullchain.pem --key=privkey.pem
```
<!-- /helm:tls -->

Those annotations are for ingress-nginx, and other controllers name them differently. F5's
NGINX Ingress Controller, for one, uses `nginx.org/` annotations. Whichever you run, set
the equivalents of:

- **No limit on the request body.** ingress-nginx's default of 1 MB refuses most image
  layers with a 413.
- **No buffering** of requests or responses.
- **Long timeouts,** enough for your largest layer on your slowest link.

The [reverse proxy guide](reverse-proxy.md) explains why each one matters.

The Ingress routes Sindri's public paths only. `/metrics`, `/healthz`, and `/readyz` carry
no authentication, so they stay inside the cluster, where Prometheus can reach them
through the Service.

## Storage

With `storage.backend: s3`, nothing else is needed. For the filesystem backend, the chart
requires a volume, because a pod's own disk does not outlive the pod:

```yaml
config:
  storage: /var/lib/sindri
persistence:
  enabled: true
  size: 200Gi
```

The volume is kept when the release is uninstalled, so `helm uninstall` cannot delete your
blobs. Delete the PersistentVolumeClaim yourself if you mean to.

## Upgrade

<!-- helm:upgrade -->
```bash
helm upgrade sindri oci://ghcr.io/spjdevops/charts/sindri --version 1.2.0 \
  --namespace sindri --values values.yaml --wait --timeout 5m
```
<!-- /helm:upgrade -->

The chart stops the old pod before starting the new one, so a push in progress during an
upgrade fails and the client has to retry it. Migrations run when the new pod starts. Read
[Upgrading](../upgrading.md) for what they guarantee, and take a
[backup](../operations.md#backup-and-restore) first.

## Scanning

Vulnerability scanning is off by default. It needs the image variant that ships Trivy, so
set `image.tag` to the `-trivy` tag and turn scanning on in `config`:

```yaml
image:
  tag: 1.2.0-trivy
config:
  scanning:
    enabled: true
```

Each hosted publish then gets a CycloneDX sbom and a Trivy scan, kept as evidence on the
artifact. Proxied content is not scanned.

Once `config.scanning` is set, the chart mounts an `emptyDir` scan cache at
`/var/cache/sindri` for Trivy's database and the scan staging area, because the root
filesystem is read-only. It is sized by the node's ephemeral storage, which must exceed the
largest image you scan. The database is downloaded again when the pod is replaced.

Trivy downloads its database from the internet on first use. For the severity gate, what
scanning covers, and mirroring the databases for an air-gapped install, see
[Scanning and the severity gate](../scanning.md).
