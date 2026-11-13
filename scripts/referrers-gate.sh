#!/usr/bin/env bash
# The referrers page's gate: runs the blocks of docs/clients/referrers.md as
# written, with oras and cosign, against a server of its own.
#
#   make up && make build && ./scripts/referrers-gate.sh
#
# Serves a fresh database and data directory on 127.0.0.1:8080. oras and cosign
# run from their pinned images, in the network namespace of a small forwarder,
# so that localhost:8080 inside them is the server on the host: the page's
# commands run unchanged, and cosign accepts plain HTTP only for localhost.
# Containers are removed with their anonymous volumes (docker rm -fv).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
PAGE="${ROOT}/docs/clients/referrers.md"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
DB="sindri_referrers_page"
BASE="http://127.0.0.1:8080"
FWD="sindri-referrers-gate"
ORAS_IMAGE="ghcr.io/oras-project/oras:v1.3.0"
COSIGN_IMAGE="ghcr.io/sigstore/cosign/cosign:v2.4.1"
SOCAT_IMAGE="alpine/socat:1.8.0.0"
SBOM="application/spdx+json"
SIGNATURE="application/vnd.dev.cosign.artifact.sig.v1+json"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

for tool in docker jq curl openssl; do
  command -v "${tool}" >/dev/null || fail "${tool} is not on PATH"
done
if curl -s -o /dev/null --max-time 2 "${BASE}/readyz"; then
  fail "something is already listening on 127.0.0.1:8080; stop it and rerun"
fi

WORK="$(mktemp -d)"
PID=""
cleanup() {
  [[ -n "${PID}" ]] && kill "${PID}" 2>/dev/null || true
  docker rm -fv "${FWD}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
docker rm -fv "${FWD}" >/dev/null 2>&1 || true

# block NAME: the lines of the page's <!-- referrers:NAME --> block, fence removed.
block() {
  local out
  out="$(awk -v n="<!-- referrers:$1 -->" -v e="<!-- /referrers:$1 -->" '$0==n{f=1;next}$0==e{f=0}f' "${PAGE}" | sed '1d;$d')"
  [ -n "${out}" ] || fail "${PAGE} has no referrers:$1 block"
  printf '%s\n' "${out}"
}

# --- the server -----------------------------------------------------------------
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
repos:
  docker:    { protocol: oci }
  other:     { protocol: oci }
  dockerhub: { upstream: dockerhub }
  oci:       { members: [docker, dockerhub] }
YAML

./bin/sindri serve --config "${WORK}/sindri.yaml" > "${WORK}/sindri.log" 2>&1 &
PID="$!"
for _ in $(seq 1 50); do
  curl -fsS "${BASE}/readyz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${BASE}/readyz" >/dev/null 2>&1 || { cat "${WORK}/sindri.log" >&2; fail "the server did not become ready"; }

# --- tools ----------------------------------------------------------------------
docker run -d --name "${FWD}" "${SOCAT_IMAGE}" \
  tcp-listen:8080,fork,reuseaddr tcp-connect:host.docker.internal:8080 >/dev/null
mkdir -p "${WORK}/bin" "${WORK}/run" "${WORK}/docker-config"
tool() {
  cat > "${WORK}/bin/$1" <<SHIM
#!/usr/bin/env bash
exec docker run --rm --network container:${FWD} -v "${WORK}/run:/run-dir" -w /run-dir \\
  -v "${WORK}/docker-config:/docker-config" -e DOCKER_CONFIG=/docker-config \\
  -e COSIGN_PASSWORD= -e COSIGN_EXPERIMENTAL "$2" "\$@"
SHIM
  chmod +x "${WORK}/bin/$1"
}
tool oras "${ORAS_IMAGE}"
tool cosign "${COSIGN_IMAGE}"
export PATH="${WORK}/bin:${PATH}"

# The page's blocks share one shell, as a reader's would: DIGEST set by the
# sign block is read by the verify and API blocks.
cd "${WORK}/run"
run() {
  step "$1"
  block "$2" > "${WORK}/block-$2.sh"
  # shellcheck disable=SC1090
  source "${WORK}/block-$2.sh"
}
api() { curl -fsS -u "token:${SINDRI_TOKEN}" "$@"; }
listed() { api "${BASE}/v2/$1/referrers/${DIGEST}" | jq -r '[.manifests[].artifactType] | sort | join(",")'; }

run "oras attaches an SBOM" attach
run "oras discover finds it" discover > "${WORK}/discover.txt"
cat "${WORK}/discover.txt"
grep -q "${SBOM}" "${WORK}/discover.txt" || fail "oras discover did not list the SBOM"

run "cosign signs, storing the signature as a referrer" sign
run "cosign verify finds the signature through the referrers API" verify

step "the API lists both, and the filter narrows it"
[ "$(listed docker/demo/app)" = "${SBOM},${SIGNATURE}" ] \
  || fail "the list is $(listed docker/demo/app), want the SBOM and the signature"
block api > "${WORK}/block-api.sh"
# shellcheck disable=SC1090
filtered="$(source "${WORK}/block-api.sh")"
[ "$(jq -r '[.manifests[].artifactType] | join(",")' <<<"${filtered}")" = "${SBOM}" ] \
  || fail "the artifactType filter returned ${filtered}"
api -D "${WORK}/headers.txt" -o /dev/null \
  "${BASE}/v2/docker/demo/app/referrers/${DIGEST}?artifactType=application/spdx%2Bjson"
grep -qi '^OCI-Filters-Applied: artifactType' "${WORK}/headers.txt" \
  || fail "a filtered answer does not say OCI-Filters-Applied"

step "no fallback tag was written: the push answered OCI-Subject"
[ "$(api "${BASE}/v2/docker/demo/app/tags/list" | jq -c .tags)" = '["1.0"]' ] \
  || fail "tags are $(api "${BASE}/v2/docker/demo/app/tags/list"), want only 1.0"

step "another repository, a proxy, and a virtual repository"
[ -z "$(listed other/demo/app)" ] || fail "another repository lists docker's referrers: $(listed other/demo/app)"
[ -z "$(listed dockerhub/demo/app)" ] || fail "a proxy lists referrers: $(listed dockerhub/demo/app)"
[ "$(listed oci/demo/app)" = "${SBOM},${SIGNATURE}" ] \
  || fail "the virtual repository lists $(listed oci/demo/app), want its hosted member's list"

step "deleting a referrer removes it from the list"
sbom_digest="$(api "${BASE}/v2/docker/demo/app/referrers/${DIGEST}?artifactType=application/spdx%2Bjson" | jq -r '.manifests[0].digest')"
api -X DELETE "${BASE}/v2/docker/demo/app/manifests/${sbom_digest}" >/dev/null
[ "$(listed docker/demo/app)" = "${SIGNATURE}" ] || fail "after the delete the list is $(listed docker/demo/app)"

echo
echo "referrers page gate passed"
