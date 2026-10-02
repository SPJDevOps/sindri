#!/usr/bin/env bash
# M7 gate: configuration as code, end to end, against the Compose stack.
#
#   make up && make build && ./scripts/m7-config-gate.sh
#
# Part 1 runs the short form the way a single team would: one file, served
# with no apply step, then a docker push into a repository nobody created by
# hand, then SIGHUP. Part 2 runs the long form the way CI would: sindri apply
# and sindri diff against a server started with no repositories.
#
# Each part gets its own fresh database. One trigger owns a row (ADR 17), so
# serve --config and apply are not pointed at the same repositories.
#
# Docker Desktop reaches the host as host.docker.internal; list it as an
# insecure registry (see scripts/oci-docker-gate.sh). Set
# REGISTRY_HOST=localhost:8080 where the daemon shares the host network.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
LONG_PORT="${LONG_PORT:-8081}"

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

PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "${p}" 2>/dev/null || true; done; }
trap cleanup EXIT

echo "== short form: a complete setup in one file, no apply step"
"${BIN}" config check -f testdata/config/sindri.yaml
"${BIN}" config show --expanded -f testdata/config/sindri.yaml

export SINDRI_DATABASE
SINDRI_DATABASE="$(fresh_db sindri_m7_short)"
SINDRI_DATA_DIR="$(mktemp -d)"
export SINDRI_DATA_DIR
"${BIN}" serve --config testdata/config/sindri.yaml > /tmp/sindri-m7-short.log 2>&1 &
SERVE_PID=$!
PIDS+=("${SERVE_PID}")
wait_ready "http://127.0.0.1:8080"
grep -q '"reconciled configuration"' /tmp/sindri-m7-short.log
echo "reconciled on boot:"
curl -fsS -H "Authorization: Bearer ${SINDRI_TOKEN}" "http://127.0.0.1:8080/api/v1/repositories" \
  | python3 -c 'import json,sys; print(" ", ", ".join(r["name"]+" ("+r["type"]+", "+r["managed_by"]+")" for r in json.load(sys.stdin)["items"]))'

CREATE_REPO=0 REPO=docker/demo/app ./scripts/oci-docker-gate.sh

kill -HUP "${SERVE_PID}"
sleep 1
test "$(grep -c '"reconciled configuration"' /tmp/sindri-m7-short.log)" -ge 2
echo "SIGHUP re-reconciled"

echo "== long form: a manifest directory from CI"
LONG_DB="$(fresh_db sindri_m7_long)"
LONG_CFG="$(mktemp)"
printf 'listen: 127.0.0.1:%s\nstorage: %s\ndatabase: %s\nauth: %s\n' \
  "${LONG_PORT}" "$(mktemp -d)" "${LONG_DB}" "${SINDRI_TOKEN}" > "${LONG_CFG}"
"${BIN}" serve --config "${LONG_CFG}" > /tmp/sindri-m7-long.log 2>&1 &
PIDS+=("$!")
wait_ready "http://127.0.0.1:${LONG_PORT}"
export SINDRI_SERVER="http://127.0.0.1:${LONG_PORT}"

"${BIN}" apply -f testdata/manifests/
"${BIN}" diff  -f testdata/manifests/
echo "diff is clean on the second run"

echo "M7 gate commands passed; now run: make test-integration"
