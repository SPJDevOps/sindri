#!/usr/bin/env bash
# Deployment gate: the Compose deployment, the reverse proxies, and backup and
# restore, as docs/deploy/compose.md, docs/deploy/reverse-proxy.md, and
# docs/operations.md write them.
#
#   ./scripts/deploy-compose-gate.sh
#
# Everything runs in a clean Docker daemon (docker:dind). Files and commands are
# read out of the pages, between their <!-- ...:... --> markers, so a page cannot
# drift from what passed.
#
# The image the page names does not exist in a registry until a release, so an
# image built from this checkout is loaded under that exact name. No file or
# command from a page is changed, with two additions for the certificates a real
# deployment already has: nginx gets a self-signed one at the page's paths, and
# Caddy gets `local_certs` so it does not ask a real CA for sindri.example.com.
#
# A restore is proved by destroying the deployment, volumes included: what was
# pushed before the backup must serve, and what was pushed after must be gone.
#
# SINDRI_GATE_IMAGE tests an image that is already built.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE_DOC="${ROOT}/docs/deploy/compose.md"
PROXY_DOC="${ROOT}/docs/deploy/reverse-proxy.md"
OPS_DOC="${ROOT}/docs/operations.md"
DIND="sindri-deploy-gate"
DIND_IMAGE="docker:29-dind"
CRANE_IMAGE="gcr.io/go-containerregistry/crane@sha256:ce0d9cffa63d62c547b7df1cad9875fe90e8f7e2413870f0b50ca03839f56901"
NGINX_IMAGE="nginx:1.28-alpine"
CADDY_IMAGE="caddy:2.10-alpine"
TRAEFIK_IMAGE="traefik:v3.5"
PROMETHEUS_IMAGE="prom/prometheus:v3.5.0"
CURL_IMAGE="curlimages/curl:8.11.1"
IMAGE="${SINDRI_GATE_IMAGE:-}"
WORK="$(mktemp -d)"

cleanup() {
  docker rm -fv "${DIND}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
docker rm -fv "${DIND}" >/dev/null 2>&1 || true

# block FILE MARKER OUTPUT: the fenced block between a page's markers.
block() {
  awk -v begin="<!-- $2 -->" -v end="<!-- /$2 -->" \
    '$0==begin{f=1;next}$0==end{f=0}f' "$1" | sed '1d;$d' > "${WORK}/$3"
  test -s "${WORK}/$3" || { echo "${1#"${ROOT}/"} has no $2 block" >&2; exit 1; }
}

echo "== extracting the pages' files and commands"
block "${COMPOSE_DOC}" compose:file compose.yaml
block "${COMPOSE_DOC}" compose:config sindri.yaml
block "${COMPOSE_DOC}" compose:up up.sh
block "${PROXY_DOC}" proxy:nginx nginx.conf
block "${PROXY_DOC}" proxy:caddy Caddyfile.page
block "${PROXY_DOC}" proxy:traefik-static traefik.yml
block "${PROXY_DOC}" proxy:traefik-dynamic dynamic.yml
block "${OPS_DOC}" ops:backup backup.sh
block "${OPS_DOC}" ops:restore restore.sh
block "${OPS_DOC}" ops:alerts alerts.yml
{ printf '{\n\tlocal_certs\n}\n\n'; cat "${WORK}/Caddyfile.page"; } > "${WORK}/Caddyfile"
rm "${WORK}/Caddyfile.page"

VERSION="$(sed -n 's/^SINDRI_VERSION=//p' "${WORK}/up.sh")"
test -n "${VERSION}" || { echo "the page sets no SINDRI_VERSION" >&2; exit 1; }

if [[ -z "${IMAGE}" ]]; then
  echo "== building the Sindri image from this checkout"
  docker build -q -t sindri:gate --build-arg VERSION=deploy-gate "${ROOT}" >/dev/null
  IMAGE=sindri:gate
fi

echo "== starting a clean Docker daemon (${DIND_IMAGE})"
docker run -d --privileged --name "${DIND}" -e DOCKER_TLS_CERTDIR= "${DIND_IMAGE}" >/dev/null
for _ in $(seq 1 60); do
  docker exec "${DIND}" docker info >/dev/null 2>&1 && break
  sleep 1
done
docker exec "${DIND}" docker info >/dev/null

echo "== loading the image as ghcr.io/spjdevops/sindri:${VERSION}, the name the page uses"
docker save "${IMAGE}" | docker exec -i "${DIND}" docker load -q >/dev/null
docker exec "${DIND}" docker tag "${IMAGE}" "ghcr.io/spjdevops/sindri:${VERSION}"

# A file rather than sh reading stdin: `docker run -i` and `docker compose exec`
# would otherwise consume the rest of the script as their own input.
cat > "${WORK}/run.sh" <<'EOS'
fail() { echo "deploy gate: $*" >&2; exit 1; }

NET=sindri_default
HOST=sindri.example.com
mkdir -p /deploy/crane
chmod 777 /deploy/crane
crane() {
  docker run --rm -i --network "${NET}" -e DOCKER_CONFIG=/cfg -v /deploy/crane:/cfg \
    "${CRANE_IMAGE}" --insecure --platform linux/amd64 "$@"
}
# status URL: the HTTP status a client gets. -k because the certificates are
# self-signed here.
status() {
  docker run --rm --network "${NET}" "${CURL_IMAGE}" -ks -o /dev/null -w '%{http_code}' "$1"
}

echo "== starting it, by the Compose page's commands"
. ./up.sh
. ./.env
printf '%s' "${SINDRI_TOKEN}" | crane auth login sindri:8080 -u admin --password-stdin 2>/dev/null

echo "== pushing and pulling directly"
crane copy alpine:3.20 sindri:8080/gate/before:1
BEFORE="$(crane digest sindri:8080/gate/before:1)"
crane validate --remote "sindri:8080/gate/before:1"

echo "== /metrics reports the queue"
docker run --rm --network "${NET}" "${CURL_IMAGE}" -fs http://sindri:8080/metrics > metrics.txt
grep -q '^sindri_queue_jobs{state="failed"} ' metrics.txt || fail "/metrics has no sindri_queue_jobs series"

echo "== the operations page's alerts parse"
docker run --rm -v /deploy/alerts.yml:/alerts.yml:ro --entrypoint promtool "${PROMETHEUS_IMAGE}" check rules /alerts.yml

mkdir -p tls
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${HOST}" \
  -keyout tls/privkey.pem -out tls/fullchain.pem 2>/dev/null
chmod 644 tls/privkey.pem

# through NAME: a push, a pull, and the hidden paths, through the proxy that is
# running under the name NAME.
through() {
  for _ in $(seq 1 30); do
    [ "$(status "https://${HOST}/v2/")" = 401 ] && break
    sleep 1
  done
  [ "$(status "https://${HOST}/v2/")" = 401 ] || { docker logs "$1" >&2; fail "$1 does not reach Sindri"; }
  printf '%s' "${SINDRI_TOKEN}" | crane auth login "${HOST}" -u admin --password-stdin 2>/dev/null
  crane copy alpine:3.20 "${HOST}/gate/$1:1"
  crane validate --remote "${HOST}/gate/$1:1"
  for path in metrics healthz readyz; do
    got="$(status "https://${HOST}/${path}")"
    [ "${got}" = 404 ] || fail "$1 serves /${path} with ${got}, want 404"
  done
  docker rm -fv "$1" >/dev/null
  echo "$1: pushed, pulled, and hides the operational paths"
}

echo "== through nginx"
docker run -d --name nginx --network "${NET}" --network-alias "${HOST}" \
  -v /deploy/nginx.conf:/etc/nginx/conf.d/default.conf:ro -v /deploy/tls:/etc/nginx/tls:ro \
  "${NGINX_IMAGE}" >/dev/null
through nginx

echo "== through Caddy"
docker run -d --name caddy --network "${NET}" --network-alias "${HOST}" \
  -v /deploy/Caddyfile:/etc/caddy/Caddyfile:ro "${CADDY_IMAGE}" >/dev/null
through caddy

echo "== through Traefik"
docker run -d --name traefik --network "${NET}" --network-alias "${HOST}" \
  -v /deploy/traefik.yml:/etc/traefik/traefik.yml:ro -v /deploy/dynamic.yml:/etc/traefik/dynamic.yml:ro \
  "${TRAEFIK_IMAGE}" >/dev/null
through traefik

echo "== backing up, by the operations page's commands"
. ./backup.sh
test -s sindri.dump || fail "the dump is empty"
test -s blobs.tar.gz || fail "the blob archive is empty"

crane copy alpine:3.20 sindri:8080/gate/after:1

echo "== destroying the deployment, volumes included"
docker compose down -v
test -z "$(docker volume ls -q)" || fail "volumes survived, so the restore would prove nothing"

echo "== restoring, by the operations page's commands"
. ./restore.sh

echo "== what the backup held still serves, and what came after is gone"
RESTORED="$(crane digest sindri:8080/gate/before:1)"
[ "${RESTORED}" = "${BEFORE}" ] || fail "restored ${RESTORED}, pushed ${BEFORE}"
crane validate --remote "sindri:8080/gate/before:1"
crane validate --remote "sindri:8080/gate/nginx:1"
if crane digest sindri:8080/gate/after:1 >/dev/null 2>&1; then
  fail "an image pushed after the backup survived the restore"
fi

echo "== the restored server accepts new pushes"
crane copy alpine:3.20 sindri:8080/gate/restored:1
crane validate --remote "sindri:8080/gate/restored:1"
EOS

docker cp "${WORK}/." "${DIND}:/deploy"
docker exec -w /deploy \
  -e CRANE_IMAGE="${CRANE_IMAGE}" -e NGINX_IMAGE="${NGINX_IMAGE}" -e CADDY_IMAGE="${CADDY_IMAGE}" \
  -e TRAEFIK_IMAGE="${TRAEFIK_IMAGE}" -e PROMETHEUS_IMAGE="${PROMETHEUS_IMAGE}" -e CURL_IMAGE="${CURL_IMAGE}" \
  "${DIND}" sh -eu run.sh

echo "Deployment gate passed: Compose, nginx, Caddy, Traefik, and backup and restore, by the pages' commands"
