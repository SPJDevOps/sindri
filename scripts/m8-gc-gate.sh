#!/usr/bin/env bash
# M8a gate: garbage collection, end to end, against the Compose stack.
#
#   make up && make build && ./scripts/m8-gc-gate.sh
#
# Three images go through a real docker push:
#
#   A  tagged 1.0                                  -> must survive
#   B  tagged 2.0, promoted to dev, then untagged   -> must survive
#   C  tagged tmp, then untagged                    -> must be collected
#
# "Untagged" is done the way it happens in practice: the tag is pushed again
# for a different image. The dry run must list C's manifest and layer and
# nothing of A or B; the collection must delete exactly those; A and B must
# still pull by digest, and a docker pull of A after removing it locally must
# work. A final dry run must list nothing.
#
# Docker Desktop reaches the host as host.docker.internal; list it as an
# insecure registry (see scripts/oci-docker-gate.sh). Set
# REGISTRY_HOST=localhost:8080 where the daemon shares the host network.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
export SINDRI_SERVER="http://127.0.0.1:8080"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
REGISTRY_HOST="${REGISTRY_HOST:-host.docker.internal:8080}"
NAME="docker/demo/gc"
IMAGE="${REGISTRY_HOST}/${NAME}"
GRACE_SECONDS=3
# Docker may push an index with attestations rather than a single manifest.
ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

fresh_db() {
  docker exec -e PGOPTIONS='--client-min-messages=warning' "${PG_CONTAINER}" psql -q -v ON_ERROR_STOP=1 -U sindri -d sindri \
    -c "DROP DATABASE IF EXISTS $1" -c "CREATE DATABASE $1" >/dev/null
  echo "postgres://sindri:sindri@127.0.0.1:${PG_PORT}/$1?sslmode=disable"
}

wait_ready() {
  for _ in $(seq 1 50); do
    curl -fsS "$1/readyz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "server at $1 did not become ready" >&2
  return 1
}

api() { curl -fsS -H "Authorization: Bearer ${SINDRI_TOKEN}" "$@"; }

# manifest_status prints the HTTP status of a manifest GET by digest.
manifest_status() {
  curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${SINDRI_TOKEN}" \
    -H "Accept: ${ACCEPT}" \
    "${SINDRI_SERVER}/v2/${NAME}/manifests/$1"
}

# build_image builds a one-file image unique to $1 and prints nothing.
build_image() {
  local ctx
  ctx="$(mktemp -d)"
  printf 'FROM scratch\nCOPY content /\n' > "${ctx}/Dockerfile"
  printf 'M8a gate image %s, built %s by %s\n' "$1" "$(date +%s)" "$$" > "${ctx}/content"
  docker build -q -t "${IMAGE}:$1" "${ctx}" >/dev/null
}

# push pushes a tag and prints the manifest digest the registry stored.
push() {
  docker push -q "${IMAGE}:$1" >/dev/null
  curl -fsS -o /dev/null -D - -H "Authorization: Bearer ${SINDRI_TOKEN}" \
    -H "Accept: ${ACCEPT}" \
    -I "${SINDRI_SERVER}/v2/${NAME}/manifests/$1" | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}'
}

PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "${p}" 2>/dev/null || true; done; }
trap cleanup EXIT

export SINDRI_DATABASE SINDRI_DATA_DIR SINDRI_GC_GRACE="${GRACE_SECONDS}s"
SINDRI_DATABASE="$(fresh_db sindri_m8_gc)"
SINDRI_DATA_DIR="$(mktemp -d)"
"${BIN}" serve --config testdata/config/sindri.yaml > /tmp/sindri-m8-gc.log 2>&1 &
PIDS+=("$!")
wait_ready "${SINDRI_SERVER}"
echo "${SINDRI_TOKEN}" | docker login "${REGISTRY_HOST}" -u gate --password-stdin >/dev/null

echo "== pushing A (kept by a tag), B (kept by a promotion), C (garbage)"
for img in a b c; do build_image "${img}"; done
docker tag "${IMAGE}:a" "${IMAGE}:1.0"
A="$(push 1.0)"

docker tag "${IMAGE}:b" "${IMAGE}:2.0"
B="$(push 2.0)"
api -X POST -H 'Content-Type: application/json' \
  -d '{"repository":"docker","stage":"dev","reason":"M8a gate"}' \
  "${SINDRI_SERVER}/api/v1/subjects/${B}/promote" >/dev/null
docker tag "${IMAGE}:a" "${IMAGE}:2.0"
test "$(push 2.0)" = "${A}"   # 2.0 now names A; B is untagged but promoted

docker tag "${IMAGE}:c" "${IMAGE}:tmp"
C="$(push tmp)"
docker tag "${IMAGE}:a" "${IMAGE}:tmp"
test "$(push tmp)" = "${A}"   # tmp now names A; C is untagged and unpromoted
echo "  A ${A}"
echo "  B ${B}"
echo "  C ${C}"

sleep $((GRACE_SECONDS + 1))

echo "== gc --dry-run"
DRY="$("${BIN}" gc --dry-run)"
echo "${DRY}"
grep -q "${C}" <<<"${DRY}" || { echo "dry run does not list C" >&2; exit 1; }
for kept in "${A}" "${B}"; do
  if grep -q "${kept}" <<<"${DRY}"; then echo "dry run lists ${kept}, which is referenced" >&2; exit 1; fi
done
test "$(manifest_status "${C}")" = 200   # a dry run deletes nothing

echo "== gc"
"${BIN}" gc

test "$(manifest_status "${A}")" = 200 || { echo "tagged A was collected" >&2; exit 1; }
test "$(manifest_status "${B}")" = 200 || { echo "promoted B was collected" >&2; exit 1; }
test "$(manifest_status "${C}")" = 404 || { echo "C survived" >&2; exit 1; }
echo "A and B survive by digest, C is gone"

docker rmi "${IMAGE}:1.0" >/dev/null
docker pull -q "${IMAGE}:1.0" >/dev/null
echo "docker pull of A works after collection"

test "$("${BIN}" gc --dry-run | grep -c '^sha256:' || true)" -eq 0 || { echo "a second dry run still lists candidates" >&2; exit 1; }
echo "a second dry run lists nothing"

echo "M8a gate commands passed; now run: make test-integration"
