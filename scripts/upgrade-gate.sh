#!/usr/bin/env bash
# Upgrade gate: the upgrade steps of docs/upgrading.md, exactly as the page
# writes them.
#
#   ./scripts/upgrade-gate.sh
#
# The commands are read out of the page, between <!-- upgrade:steps --> and
# <!-- /upgrade:steps -->, so the page cannot drift from what passed. The
# block has two halves, split at its "# Compose quickstart" and "# Binary"
# comments, and each is run where it applies:
#
# Compose: in a clean Docker daemon (docker:dind), the quickstart's Compose
# file starts on the latest release, an image is pushed, and then the page's
# command upgrades it to an image built from this checkout, tagged as version
# "gate". The gate passes when the new container is healthy, logs
# "database schema is up to date" as the page says to check, and still serves
# the image pushed before the upgrade.
#
# Binary: in a throwaway linux/amd64 container, the page's tar and install
# lines run against the latest release's real archive, downloaded and checked
# against the release's checksums.txt. systemctl is not there, so it is
# defined as what the restarted service would first do: run the installed
# binary. This checks the archive name and layout the page promises.
#
# SINDRI_GATE_PREVIOUS names the release to upgrade from (default: the latest
# tag reachable from HEAD). SINDRI_GATE_IMAGE tests an image that is already
# built instead of building one.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOC="${ROOT}/docs/upgrading.md"
DIND="sindri-upgrade-gate"
DIND_IMAGE="docker:29-dind"
TOOLS_IMAGE="alpine:3.20"
# On a release commit the latest tag is HEAD's own, so look from its parent.
if git -C "${ROOT}" describe --tags --exact-match HEAD >/dev/null 2>&1; then
  FROM="HEAD^"
else
  FROM="HEAD"
fi
PREVIOUS="${SINDRI_GATE_PREVIOUS:-$(git -C "${ROOT}" describe --tags --abbrev=0 "${FROM}")}"
PREVIOUS="${PREVIOUS#v}"
IMAGE="${SINDRI_GATE_IMAGE:-}"
REPO_IMAGE="ghcr.io/spjdevops/sindri"

cleanup() { docker rm -fv "${DIND}" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "== extracting the upgrade steps from ${DOC#"${ROOT}/"}"
WORK="$(mktemp -d)"
awk '/<!-- upgrade:steps -->/{f=1;next}/<!-- \/upgrade:steps -->/{f=0}f' "${DOC}" \
  | sed '1d;$d' | sed 's/^   //' > "${WORK}/steps.sh"
awk '/^# Compose quickstart/{f=1;next}/^# Binary/{f=0}f' "${WORK}/steps.sh" > "${WORK}/compose.sh"
awk '/^# Binary/{f=1;next}f' "${WORK}/steps.sh" > "${WORK}/binary.sh"
grep -q 'docker compose' "${WORK}/compose.sh" || { echo "no Compose step in the upgrade block" >&2; exit 1; }
grep -q 'install' "${WORK}/binary.sh" || { echo "no binary step in the upgrade block" >&2; exit 1; }
sed -i.bak "s/<version>/gate/g" "${WORK}/compose.sh"
sed -i.bak "s/<version>/${PREVIOUS}/g" "${WORK}/binary.sh"
echo "upgrading from ${PREVIOUS}"

echo "== binary: the page's install lines against sindri ${PREVIOUS}'s release archive"
ARCHIVE="sindri_${PREVIOUS}_linux_amd64.tar.gz"
BASE="https://github.com/SPJDevOps/sindri/releases/download/v${PREVIOUS}"
curl -fsSL --retry 3 -o "${WORK}/${ARCHIVE}" "${BASE}/${ARCHIVE}"
curl -fsSL --retry 3 -o "${WORK}/checksums.txt" "${BASE}/checksums.txt"
# Only the stubbed systemctl writes to binary.out, so the check below proves
# the page's last line ran the binary its install line put in place.
docker run --rm --platform linux/amd64 -v "${WORK}:/work" -w /work "${TOOLS_IMAGE}" sh -euc '
  grep " '"${ARCHIVE}"'\$" checksums.txt | sha256sum -c - >&2
  systemctl() { /usr/local/bin/sindri version > /work/binary.out; }
  set -x
  . ./binary.sh
'
cat "${WORK}/binary.out"
grep -qF "sindri ${PREVIOUS} (" "${WORK}/binary.out" || { echo "the installed binary is not ${PREVIOUS}" >&2; exit 1; }
echo "the page's install lines installed sindri ${PREVIOUS}"

if [[ -z "${IMAGE}" ]]; then
  echo "== building the Sindri image from this checkout"
  docker build -q -t sindri:gate --build-arg VERSION=upgrade-gate "${ROOT}" >/dev/null
  IMAGE=sindri:gate
fi

echo "== starting a clean Docker daemon (${DIND_IMAGE})"
docker run -d --privileged --name "${DIND}" -e DOCKER_TLS_CERTDIR= "${DIND_IMAGE}" >/dev/null
for _ in $(seq 1 60); do
  docker exec "${DIND}" docker info >/dev/null 2>&1 && break
  sleep 1
done
docker exec "${DIND}" docker info >/dev/null

echo "== loading ${IMAGE} into it as ${REPO_IMAGE}:gate"
docker save "${IMAGE}" | docker exec -i "${DIND}" docker load -q >/dev/null
docker exec "${DIND}" docker tag "${IMAGE}" "${REPO_IMAGE}:gate"

docker exec "${DIND}" mkdir -p /quickstart
# A user upgrades by keeping the Compose file they have and changing the image,
# so the gate starts from the release's own file, not this checkout's.
git -C "${ROOT}" show "v${PREVIOUS}:deploy/quickstart/compose.yaml" > "${WORK}/compose.yaml"
docker cp "${WORK}/compose.yaml" "${DIND}:/quickstart/compose.yaml"
docker cp "${WORK}/compose.sh" "${DIND}:/quickstart/upgrade.sh"

echo "== Compose: start ${PREVIOUS}, push an image, upgrade with the page's command"
docker exec -i -w /quickstart -e PREVIOUS="${PREVIOUS}" -e REPO_IMAGE="${REPO_IMAGE}" "${DIND}" sh -eu <<'EOS'
export SINDRI_TOKEN=upgrade-gate-token
SINDRI_IMAGE="${REPO_IMAGE}:${PREVIOUS}" docker compose up -d --wait
old=$(docker compose ps -q sindri)
echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin
docker pull -q alpine:3.20
docker tag alpine:3.20 localhost:8080/demo/app:1.0
docker push -q localhost:8080/demo/app:1.0

set -x
. ./upgrade.sh
set +x

for _ in $(seq 1 60); do
  [ "$(docker compose ps sindri --format '{{.Health}}')" = healthy ] && break
  sleep 2
done
new=$(docker compose ps -q sindri)
[ "$new" != "$old" ] || { echo "the page's command did not replace the container" >&2; exit 1; }
[ "$(docker inspect -f '{{.Config.Image}}' "$new")" = "${REPO_IMAGE}:gate" ] \
  || { echo "the new container does not run ${REPO_IMAGE}:gate" >&2; exit 1; }
[ "$(docker compose ps sindri --format '{{.Health}}')" = healthy ] \
  || { echo "the upgraded server is not healthy" >&2; docker compose logs sindri >&2; exit 1; }
docker logs "$new" 2>&1 | grep -q 'database schema is up to date' \
  || { echo "the upgraded server did not log 'database schema is up to date'" >&2; docker logs "$new" >&2; exit 1; }
echo "the upgraded server logged: $(docker logs "$new" 2>&1 | grep 'database schema is up to date')"

docker rmi -f localhost:8080/demo/app:1.0 >/dev/null
docker pull -q localhost:8080/demo/app:1.0
echo "the image pushed to ${PREVIOUS} pulls from the upgraded server"
docker compose down -v
EOS

echo "Upgrade gate passed: docs/upgrading.md upgrades ${PREVIOUS} to this checkout"
