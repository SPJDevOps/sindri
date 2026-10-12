#!/usr/bin/env bash
# M8b gate: the quickstart, exactly as docs/quickstart.md writes it, on a
# machine with nothing but Docker.
#
#   ./scripts/m8-quickstart-gate.sh
#
# "A machine with nothing but Docker" is a fresh docker:dind container: its
# own daemon, no Go, no images, no build cache, no registry logins. The
# Compose file and the commands are read out of the document, not copied
# into this script, so the document cannot drift from what passed.
#
# The one thing not taken from the internet is the Sindri image, which does
# not exist in a registry until a release: it is built here, from this
# checkout, and loaded into the clean daemon as sindri:gate. Set
# SINDRI_GATE_IMAGE to test an image that is already built, such as one from
# `goreleaser release --snapshot`.
#
# `open` has no browser to open here, so it is defined as what a browser
# does: follow /auth/login through the identity provider and back, keeping
# cookies, and fetch the UI. The gate passes when the signed-in page lists the
# image the document pushed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOC="${ROOT}/docs/quickstart.md"
DIND="sindri-quickstart-gate"
DIND_IMAGE="docker:29-dind"
IMAGE="${SINDRI_GATE_IMAGE:-}"

cleanup() { docker rm -f "${DIND}" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "== extracting the quickstart from ${DOC#"${ROOT}/"}"
WORK="$(mktemp -d)"
awk '/<!-- quickstart:compose -->/{f=1;next}/<!-- \/quickstart:compose -->/{f=0}f' "${DOC}" | sed '1d;$d' > "${WORK}/compose.yaml"
awk '/<!-- quickstart:begin -->/{f=1;next}/<!-- quickstart:end -->/{f=0}f' "${DOC}" | sed '1d;$d' > "${WORK}/steps.sh"
test -s "${WORK}/compose.yaml" || { echo "no compose block in the quickstart" >&2; exit 1; }
test -s "${WORK}/steps.sh" || { echo "no command block in the quickstart" >&2; exit 1; }
diff -u "${ROOT}/deploy/quickstart/compose.yaml" "${WORK}/compose.yaml" \
  || { echo "docs/quickstart.md and deploy/quickstart/compose.yaml have drifted apart" >&2; exit 1; }
echo "the document's Compose file matches deploy/quickstart/compose.yaml"

if [[ -z "${IMAGE}" ]]; then
  echo "== building the Sindri image from this checkout"
  docker build -q -t sindri:gate --build-arg VERSION=quickstart-gate "${ROOT}" >/dev/null
  IMAGE=sindri:gate
fi

echo "== starting a clean Docker daemon (${DIND_IMAGE})"
docker run -d --privileged --name "${DIND}" -e DOCKER_TLS_CERTDIR= "${DIND_IMAGE}" >/dev/null
for _ in $(seq 1 60); do
  docker exec "${DIND}" docker info >/dev/null 2>&1 && break
  sleep 1
done
docker exec "${DIND}" docker info >/dev/null
echo "images in the clean daemon before the quickstart: $(docker exec "${DIND}" docker images -q | wc -l | tr -d ' ')"

echo "== loading ${IMAGE} into it as sindri:gate"
docker save "${IMAGE}" | docker exec -i "${DIND}" docker load -q >/dev/null
if [[ "${IMAGE}" != "sindri:gate" ]]; then
  docker exec "${DIND}" docker tag "${IMAGE}" sindri:gate
fi

docker exec "${DIND}" mkdir -p /quickstart
docker cp "${WORK}/compose.yaml" "${DIND}:/quickstart/compose.yaml"
docker cp "${WORK}/steps.sh" "${DIND}:/quickstart/steps.sh"

echo "== running the quickstart's commands, verbatim"
docker exec -i -w /quickstart "${DIND}" sh -eu <<'EOS'
export SINDRI_IMAGE=sindri:gate
# What a browser does with `open http://localhost:8080/ui/`: sign in through
# the identity provider, keeping cookies, and fetch the page.
open() {
  docker run --rm --network host curlimages/curl:8.11.1 \
    -fsSL -c /tmp/jar -b /tmp/jar "http://localhost:8080/auth/login?next=${1#http://localhost:8080}" > /quickstart/ui.html
}
set -x
. ./steps.sh
set +x
grep -q 'href="/ui/r/demo/app"' /quickstart/ui.html || { echo "the signed-in UI does not list demo/app" >&2; cat /quickstart/ui.html >&2; exit 1; }
grep -q 'Signed in as user:' /quickstart/ui.html || { echo "the UI page is not a signed-in one" >&2; exit 1; }
echo "the signed-in UI lists demo/app"
docker compose ps --format '{{.Service}}: {{.State}} {{.Health}}'
EOS

echo "M8b quickstart gate passed: docs/quickstart.md works on a clean Docker daemon"
