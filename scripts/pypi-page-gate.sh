#!/usr/bin/env bash
# The pip page's gate: scripts/pypi-twine-gate.sh, which publishes and installs
# as docs/clients/pip.md describes, against a server of its own.
#
#   make up && make build && ./scripts/pypi-page-gate.sh
#
# Serves a fresh database and data directory, with twine and build in a
# throwaway virtual environment. Fails if the page stops showing the commands.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
PAGE="docs/clients/pip.md"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
DB="sindri_pypi_page"
BASE="http://127.0.0.1:8080"

fail() { echo "FAIL: $*" >&2; exit 1; }

if curl -s -o /dev/null --max-time 2 "${BASE}/readyz"; then
  fail "something is already listening on 127.0.0.1:8080; stop it and rerun"
fi

for line in \
  'python -m twine upload \' \
  '  --repository-url http://127.0.0.1:8080/pypi/internal/upload \' \
  '  -u token -p "$SINDRI_TOKEN" dist/*' \
  'pip install --index-url "http://token:$SINDRI_TOKEN@127.0.0.1:8080/pypi/internal/simple/" \'; do
  grep -Fxq -- "${line}" "${PAGE}" || fail "${PAGE} does not contain the line: ${line}"
done

WORK="$(mktemp -d)"
PID=""
cleanup() {
  [[ -n "${PID}" ]] && kill "${PID}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

python3 -m venv "${WORK}/venv"
"${WORK}/venv/bin/pip" install -q twine build
export PATH="${WORK}/venv/bin:${PATH}"

docker exec -e PGOPTIONS='--client-min-messages=warning' "${PG_CONTAINER}" psql -q -v ON_ERROR_STOP=1 -U sindri -d sindri \
  -c "DROP DATABASE IF EXISTS ${DB}" -c "CREATE DATABASE ${DB}" >/dev/null

SINDRI_TOKEN="$(openssl rand -hex 32)"
export SINDRI_TOKEN
cat > "${WORK}/sindri.yaml" <<YAML
listen: 127.0.0.1:8080
storage: ${WORK}/data
database: postgres://sindri:sindri@127.0.0.1:${PG_PORT}/${DB}?sslmode=disable
auth:
  static_token: \${SINDRI_TOKEN}
YAML

./bin/sindri serve --config "${WORK}/sindri.yaml" > "${WORK}/sindri.log" 2>&1 &
PID="$!"
for _ in $(seq 1 50); do
  curl -fsS "${BASE}/readyz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${BASE}/readyz" >/dev/null 2>&1 || { cat "${WORK}/sindri.log" >&2; fail "the server did not become ready"; }

./scripts/pypi-twine-gate.sh
echo "pip page gate passed"
