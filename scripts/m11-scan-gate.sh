#!/usr/bin/env bash
# M11a scan gate: scanning attaches an sbom and a scan to what is published, a
# severity gate refuses promotion above its ceiling and names the CVEs, and
# scanning still works on a server with no route to the public databases.
#
#   make up && make build && ./scripts/m11-scan-gate.sh
#
# What it does, against the -trivy image built from deploy/docker/Dockerfile.trivy
# (not the host binary: the Trivy that ships is the Trivy that is tested):
#
#   1. Builds the image for the daemon's architecture.
#   2. Starts Sindri with Postgres and MinIO on a Docker network and runs the
#      docs/scanning.md blocks (push, wait, promote, evidence, staleness) against
#      it: an old alpine image is refused with CVE-2021-36159 named.
#   3. Publishes a wheel and an npm tarball that pin a vulnerable package
#      (ADR 29 depth): both are refused at a medium ceiling with CVEs named, and
#      the wheel, whose findings sit at or below `high`, passes a high ceiling.
#   4. Runs the page's mirror and pre-seed blocks.
#   5. Air-gapped: restarts everything on a network created with --internal,
#      with the mirror (behind a TLS listener, because Trivy insists on HTTPS for
#      any host but localhost) reachable and ghcr.io not, Sindri pointed at the
#      mirror for both databases, and shows a scan of an image containing a jar.
#   6. Runs the page's pre-seeded-cache variant (skipDBUpdate) the same way.
#
# It needs Docker, go, jq, python3, node and npm, and network access to ghcr.io
# and Docker Hub for the first part. The development stack (make up) is not
# used; the gate starts its own Postgres and MinIO, from the images that
# deploy/compose/docker-compose.yml pins, so ports 8080 and 5001 must be free.
# `oras` is used when it is on PATH; otherwise the pinned oras image runs in the
# mirror's network namespace, where the page's localhost:5000 is the mirror.
#
# Containers are removed with their anonymous volumes (docker rm -fv): the mirror
# holds both databases, about 1 GiB, and Postgres and MinIO declare volumes too.
# A run that leaked them would fill the Docker VM's disk after a few runs.
#
# The Java database is about 950 MiB; it is downloaded twice (mirror copy and
# pre-seed). Set SINDRI_GATE_CACHE to a directory to keep the downloads between
# runs of this gate; the pre-seed block still runs against it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
PAGE="${ROOT}/docs/scanning.md"
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
COMPOSE="${ROOT}/deploy/compose/docker-compose.yml"
ORAS_IMAGE="ghcr.io/oras-project/oras:v1.3.0"
SOCAT_IMAGE="alpine/socat:1.8.0.3"
REGISTRY_IMAGE="registry:3.0.0"
IMAGE="sindri:m11-trivy"
SERVER="sindri-m11"
OPEN_NET="m11-open"
AIR_NET="m11-air"
SITE="http://localhost:8080"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
image_of() { sed -n "s|^ *image: *\(${1}[^ ]*\) *\$|\1|p" "${COMPOSE}" | head -1; }
PG_IMAGE="$(image_of postgres)"
MINIO_IMAGE="$(image_of bitnamilegacy/minio:)"
MC_IMAGE="$(image_of bitnamilegacy/minio-client)"
[ -n "${PG_IMAGE}" ] && [ -n "${MINIO_IMAGE}" ] && [ -n "${MC_IMAGE}" ] ||
  fail "could not read the Postgres and MinIO images from ${COMPOSE}"

for tool in docker go jq python3 node npm curl; do
  command -v "${tool}" >/dev/null || fail "${tool} is not on PATH"
done
for port in 8080 5001; do
  if curl -s -o /dev/null --max-time 2 "http://localhost:${port}/"; then
    fail "something is already listening on localhost:${port}; stop it and rerun"
  fi
done

WORK="$(mktemp -d)"
cleanup() {
  status=$?
  if [ "${status}" -ne 0 ]; then
    docker logs "${SERVER}" 2>&1 | grep -v '"msg":"request"' | tail -40 >&2 || true
  fi
  docker rm -fv "${SERVER}" "${SERVER}-pg" "${SERVER}-minio" "${SERVER}-proxy" trivy-mirror trivy-mirror-tls >/dev/null 2>&1 || true
  docker network rm "${OPEN_NET}" "${AIR_NET}" >/dev/null 2>&1 || true
  docker volume rm -f sindri-m11-trivy-cache >/dev/null 2>&1 || true
  docker logout localhost:8080 >/dev/null 2>&1 || true
  docker rmi -f localhost:8080/scan/old:1 localhost:8080/scan/jar:1 >/dev/null 2>&1 || true
  rm -rf "${WORK}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Leftovers of a killed earlier run.
docker rm -fv "${SERVER}" "${SERVER}-pg" "${SERVER}-minio" "${SERVER}-proxy" trivy-mirror trivy-mirror-tls >/dev/null 2>&1 || true
docker network rm "${OPEN_NET}" "${AIR_NET}" >/dev/null 2>&1 || true

# --- page blocks --------------------------------------------------------------
# block NAME: the lines of the page's <!-- scan:NAME --> block, fence removed.
block() {
  local out
  out="$(awk -v n="<!-- scan:$1 -->" -v e="<!-- /scan:$1 -->" '$0==n{f=1;next}$0==e{f=0}f' "${PAGE}" | sed '1d;$d')"
  [ -n "${out}" ] || fail "${PAGE} has no scan:$1 block"
  printf '%s\n' "${out}"
}
# run_block NAME: runs the page's block as written in its own shell. Pages are
# pasted into a plain shell, so only -u and pipefail are set.
run_block() {
  block "$1" > "${WORK}/block-$1.sh"
  bash -uo pipefail "${WORK}/block-$1.sh"
}

# --- tools --------------------------------------------------------------------
mkdir -p "${WORK}/bin"
export PATH="${WORK}/bin:${PATH}"
if ! command -v oras >/dev/null; then
  cat > "${WORK}/bin/oras" <<EOF
#!/usr/bin/env bash
# oras from its pinned image, in the mirror's network namespace so that
# localhost:5000 is the mirror and ghcr.io is reached through the mirror's route.
# The page publishes the mirror on 5001 for the host, so that port is mapped back.
exec docker run --rm --network container:trivy-mirror "${ORAS_IMAGE}" "\${@//localhost:5001/localhost:5000}"
EOF
  chmod +x "${WORK}/bin/oras"
fi

# --- the image ----------------------------------------------------------------
step "build the -trivy image from deploy/docker/Dockerfile.trivy"
ARCH="$(docker version --format '{{.Server.Arch}}')"
PLATFORM="linux/${ARCH}"
CTX="${WORK}/ctx"
mkdir -p "${CTX}/${PLATFORM}" "${CTX}/deploy/docker"
CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${CTX}/${PLATFORM}/sindri" ./cmd/sindri
cp -R deploy/docker/data "${CTX}/deploy/docker/data"
docker build -q --platform "${PLATFORM}" -f deploy/docker/Dockerfile.trivy -t "${IMAGE}" "${CTX}" >/dev/null
echo "built ${IMAGE} for ${PLATFORM}"

# --- the stack ----------------------------------------------------------------
# write_config FILE SCANNING_YAML: the server's configuration. The repositories
# and the lifecycle are the ones the page declares, plus the strict variants.
write_config() {
  cat > "$1" <<EOF
listen: :8080
storage: {backend: s3, bucket: sindri-blobs, region: us-east-1, endpoint: "http://${SERVER}-minio:9000", pathStyle: true}
database: postgres://sindri:sindri@${SERVER}-pg:5432/sindri?sslmode=disable
auth: ${SINDRI_TOKEN}
publicUrl: ${SITE}
log: info
scanning:
$2
repos:
  scan: {protocol: oci, lifecycle: [dev, prod]}
  py: {protocol: pypi, lifecycle: [dev, prod]}
  py-strict: {protocol: pypi, lifecycle: [dev, staging]}
  js-strict: {protocol: npm, lifecycle: [dev, staging]}
lifecycle:
  stages: [dev, staging, prod]
  gates:
    staging:
      - scan: {maxSeverity: medium}
    prod:
      - scan: {maxSeverity: high}
EOF
}

# start_data NET: Postgres and MinIO with the bucket, on NET.
start_data() {
  docker run -d --name "${SERVER}-pg" --network "$1" --network-alias "${SERVER}-pg" \
    -e POSTGRES_USER=sindri -e POSTGRES_PASSWORD=sindri -e POSTGRES_DB=sindri "${PG_IMAGE}" >/dev/null
  docker run -d --name "${SERVER}-minio" --network "$1" --network-alias "${SERVER}-minio" \
    -e MINIO_ROOT_USER=sindri -e MINIO_ROOT_PASSWORD=sindri-dev-secret \
    -e MINIO_API_PORT_NUMBER=9000 -e MINIO_CONSOLE_PORT_NUMBER=9001 "${MINIO_IMAGE}" >/dev/null
  for _ in $(seq 1 60); do
    docker exec "${SERVER}-pg" pg_isready -q -U sindri -d sindri && break
    sleep 1
  done
  for _ in $(seq 1 60); do
    docker exec "${SERVER}-minio" mc ready local >/dev/null 2>&1 && break
    sleep 1
  done
  docker run --rm --network "$1" --entrypoint /bin/sh "${MC_IMAGE}" -c \
    "mc alias set local http://${SERVER}-minio:9000 sindri sindri-dev-secret >/dev/null && mc mb --ignore-existing local/sindri-blobs" >/dev/null
}

# start_sindri NET CONFIG [docker run flags...]: the server, on NET.
start_sindri() {
  local net="$1" cfg="$2"
  shift 2
  docker run -d --name "${SERVER}" --network "${net}" \
    -e AWS_ACCESS_KEY_ID=sindri -e AWS_SECRET_ACCESS_KEY=sindri-dev-secret -e AWS_REGION=us-east-1 \
    -v "${cfg}:/etc/sindri/sindri.yaml:ro" "$@" "${IMAGE}" >/dev/null
}

wait_ready() {
  for _ in $(seq 1 90); do
    curl -fsS "${SITE}/readyz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  fail "the server did not become ready"
}

# start_proxy: publishes the host's 8080 for the server on AIR_NET. Docker publishes
# no ports from an --internal network, so the forwarder starts on a normal one and
# is then connected to the internal one. The server itself is only on AIR_NET.
start_proxy() {
  docker run -d --name "${SERVER}-proxy" --network "${OPEN_NET}" -p 127.0.0.1:8080:8080 "${SOCAT_IMAGE}" \
    TCP-LISTEN:8080,fork,reuseaddr "TCP:${SERVER}:8080" >/dev/null
  docker network connect "${AIR_NET}" "${SERVER}-proxy"
}

teardown_stack() {
  docker rm -fv "${SERVER}" "${SERVER}-pg" "${SERVER}-minio" "${SERVER}-proxy" >/dev/null 2>&1 || true
}

AUTH=(-H "Authorization: Bearer ${SINDRI_TOKEN}")
api() { curl -fsS "${AUTH[@]}" "$@"; }

# wait_evidence DIGEST: waits for a scan row; prints the evidence list.
wait_scan() {
  local n=0
  for _ in $(seq 1 150); do
    n="$(api "${SITE}/api/v1/subjects/$1/evidence" | jq '[.items[] | select(.type == "scan")] | length')"
    [ "${n}" -gt 0 ] && return 0
    sleep 2
  done
  fail "no scan evidence for $1 after five minutes"
}

# promote DIGEST REPO STAGE: prints the HTTP status, the body in WORK/promote.json.
promote() {
  curl -sS -o "${WORK}/promote.json" -w '%{http_code}' -X POST "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"repository\":\"$2\",\"stage\":\"$3\",\"reason\":\"m11 gate\"}" \
    "${SITE}/api/v1/subjects/$1/promote"
}
expect_refused() { # DIGEST REPO STAGE [CVE]
  local code
  code="$(promote "$1" "$2" "$3")"
  [ "${code}" = 409 ] || { cat "${WORK}/promote.json" >&2; fail "promote $2 to $3 answered ${code}, want 409"; }
  jq -e '.code == "GATE_FAILED"' "${WORK}/promote.json" >/dev/null || fail "the refusal is not GATE_FAILED"
  jq -e '[.violations[].findings[]?.id] | length > 0 and all(test("^(CVE|GHSA)-"))' "${WORK}/promote.json" >/dev/null ||
    fail "the refusal names no CVE: $(cat "${WORK}/promote.json")"
  if [ -n "${4:-}" ]; then
    jq -e --arg cve "$4" '[.violations[].findings[]?.id] | index($cve) != null' "${WORK}/promote.json" >/dev/null ||
      fail "the refusal does not name $4: $(cat "${WORK}/promote.json")"
  fi
  echo "  refused (409), CVEs named: $(jq -r '[.violations[].findings[]?.id] | unique | join(", ")' "${WORK}/promote.json" | cut -c1-120)"
}

# --- 1. images, wheels and tarballs ---------------------------------------------
step "start Sindri (${IMAGE}) with scanning on"
docker network create "${OPEN_NET}" >/dev/null
write_config "${WORK}/open.yaml" "  enabled: true"
start_data "${OPEN_NET}"
start_sindri "${OPEN_NET}" "${WORK}/open.yaml" -p 127.0.0.1:8080:8080
wait_ready
echo "ready"

step "the page: push an old image, wait for its scan, promote it"
DIGEST="$(run_block push | tail -1)"
case "${DIGEST}" in sha256:????????????????????????????????????????????????????????????????) ;; *) fail "the push block printed no digest: '${DIGEST}'" ;; esac
echo "digest ${DIGEST}"
export DIGEST
run_block wait | tee "${WORK}/wait.out"
grep -q '^scans: [1-9]' "${WORK}/wait.out" || fail "the wait block saw no scan"
types="$(api "${SITE}/api/v1/subjects/${DIGEST}/evidence" | jq -r '[.items[].type] | sort | join(",")')"
[ "${types}" = "sbom,scan" ] || fail "evidence types are '${types}', want sbom,scan"
echo "evidence: ${types}"

run_block promote | tee "${WORK}/promote.out"
jq -e '.code == "GATE_FAILED"' "${WORK}/promote.out" >/dev/null || fail "the promote block did not show GATE_FAILED"
jq -e '[.violations[].findings[].id] | index("CVE-2021-36159") != null' "${WORK}/promote.out" >/dev/null ||
  fail "the refusal does not name CVE-2021-36159"
jq -e '.violations[0].rule == "scan.max_severity=high"' "${WORK}/promote.out" >/dev/null || fail "the violation's rule is not scan.max_severity=high"
echo "image refused at the high ceiling, CVE-2021-36159 named"

run_block evidence | tee "${WORK}/evidence.out"
jq -es 'length == 1 and .[0].metadata.producer == "sindri" and .[0].metadata.coverage == "image"
        and (.[0].metadata.vulnerabilities | map(.id) | index("CVE-2021-36159") != null)' "${WORK}/evidence.out" >/dev/null ||
  fail "the evidence block does not show a Sindri image scan listing CVE-2021-36159"
run_block staleness | tee "${WORK}/staleness.out"
grep -Eq '^database is [0-9]+ days old$' "${WORK}/staleness.out" || fail "the staleness block printed no age"

step "a wheel: a vendored Jinja2 2.10 inside the archive"
python3 -I - "${WORK}" <<'PY'
import hashlib, sys, zipfile
work = sys.argv[1]
def wheel(name, version, vendored):
    path = f"{work}/{name}-{version}-py3-none-any.whl"
    di = f"{name}-{version}.dist-info"
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr(f"{name}/__init__.py", "")
        z.writestr(f"{di}/METADATA", f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n")
        z.writestr(f"{di}/WHEEL", "Wheel-Version: 1.0\nGenerator: m11-gate\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
        # A vendored dist-info: the wheel pins this version inside itself, which is
        # the depth a wheel scan reaches (ADR 29).
        for vname, vver in vendored:
            z.writestr(f"{name}/_vendor/{vname}-{vver}.dist-info/METADATA",
                       f"Metadata-Version: 2.1\nName: {vname}\nVersion: {vver}\n")
        z.writestr(f"{di}/RECORD", "")
    return path
wheel("m11demo", "1.0", [("Jinja2", "2.10")])
PY
WHEEL="${WORK}/m11demo-1.0-py3-none-any.whl"
WHEEL_DIGEST="sha256:$(shasum -a 256 "${WHEEL}" | cut -d' ' -f1)"
upload_wheel() { # REPO
  curl -fsS -o /dev/null -X POST -u "token:${SINDRI_TOKEN}" "${SITE}/pypi/$1/upload" \
    -F ':action=file_upload' -F 'name=m11demo' -F 'version=1.0' -F "content=@${WHEEL}"
}
upload_wheel py-strict
upload_wheel py
wait_scan "${WHEEL_DIGEST}"
api "${SITE}/api/v1/subjects/${WHEEL_DIGEST}/evidence" |
  jq -e '[.items[] | select(.type == "scan")][0].metadata | .coverage == "package" and .producer == "sindri" and (.vulnerabilities | length > 0)' >/dev/null ||
  fail "the wheel's scan is not a Sindri package scan with findings"
api "${SITE}/api/v1/subjects/${WHEEL_DIGEST}/evidence" | jq -r '[.items[] | select(.type == "scan")][0].metadata.severity_counts | tojson' |
  sed 's/^/  wheel severity counts: /'
echo "wheel: sbom and scan present"
expect_refused "${WHEEL_DIGEST}" py-strict staging
# The same wheel has findings, none above high, so the high ceiling passes it.
crit="$(api "${SITE}/api/v1/subjects/${WHEEL_DIGEST}/evidence" | jq '[.items[] | select(.type == "scan")][0].metadata.severity_counts | .CRITICAL + .UNKNOWN')"
[ "${crit}" = 0 ] || fail "the wheel fixture has ${crit} critical or unknown findings; choose another vendored package"
code="$(promote "${WHEEL_DIGEST}" py prod)"
[ "${code}" = 201 ] || { cat "${WORK}/promote.json" >&2; fail "promote of the wheel to prod answered ${code}, want 201"; }
echo "wheel with findings at or below high promoted to prod (201)"
api "${SITE}/api/v1/subjects/${WHEEL_DIGEST}/events" | jq -e '[.items[] | select((.evidence_ids // []) | length > 0)] | length > 0' >/dev/null ||
  fail "the ledger event for the passing promotion carries no evidence_ids"
echo "  the ledger event records the evidence ids"

step "an npm tarball: a bundled minimist 0.0.8"
# npm pack bundles node_modules/minimist into the tarball, as the scanner's own
# test fixture does.
mkdir -p "${WORK}/npm/node_modules/minimist"
printf '{"name":"m11-bundler","version":"1.0.0","dependencies":{"minimist":"0.0.8"},"bundleDependencies":["minimist"]}\n' > "${WORK}/npm/package.json"
printf '{"name":"minimist","version":"0.0.8"}\n' > "${WORK}/npm/node_modules/minimist/package.json"
export npm_config_userconfig="${WORK}/user.npmrc" npm_config_globalconfig="${WORK}/global.npmrc"
export npm_config_cache="${WORK}/npm-cache" npm_config_update_notifier=false npm_config_fund=false npm_config_loglevel=warn
printf '//localhost:8080/npm/js-strict/:_authToken=%s\n' "${SINDRI_TOKEN}" > "${npm_config_userconfig}"
: > "${npm_config_globalconfig}"
(cd "${WORK}/npm" && npm pack --silent --pack-destination "${WORK}" >/dev/null)
npm publish "${WORK}/m11-bundler-1.0.0.tgz" --registry "${SITE}/npm/js-strict/" >/dev/null
TGZ_DIGEST="sha256:$(shasum -a 256 "${WORK}/m11-bundler-1.0.0.tgz" | cut -d' ' -f1)"
wait_scan "${TGZ_DIGEST}"
types="$(api "${SITE}/api/v1/subjects/${TGZ_DIGEST}/evidence" | jq -r '[.items[].type] | sort | join(",")')"
[ "${types}" = "sbom,scan" ] || fail "tarball evidence types are '${types}', want sbom,scan"
echo "tarball: sbom and scan present"
expect_refused "${TGZ_DIGEST}" js-strict staging

# --- 2. the page's mirror and pre-seed blocks -----------------------------------
step "the page: a throwaway mirror, and both databases copied into it"
block mirror-registry | sed "s#registry:3\$#${REGISTRY_IMAGE}#" > "${WORK}/block-mirror-registry.sh"
grep -q "${REGISTRY_IMAGE}" "${WORK}/block-mirror-registry.sh" || fail "the mirror-registry block does not use registry:3; update the pin in this script"
bash -uo pipefail "${WORK}/block-mirror-registry.sh"
for _ in $(seq 1 30); do curl -fsS http://localhost:5001/v2/ >/dev/null 2>&1 && break; sleep 1; done
export MIRROR=localhost:5001
run_block mirror
oras repo tags --plain-http "${MIRROR}/trivy/trivy-db" | grep -qx 2 || fail "the mirror has no trivy-db:2"
oras repo tags --plain-http "${MIRROR}/trivy/trivy-java-db" | grep -qx 1 || fail "the mirror has no trivy-java-db:1"
echo "mirror holds trivy-db:2 and trivy-java-db:1"

step "the page: pre-seed a cache directory with both databases"
mkdir -p "${WORK}/preseed"
(
  cd "${WORK}/preseed"
  if [ -n "${SINDRI_GATE_CACHE:-}" ] && [ -f "${SINDRI_GATE_CACHE}/trivy-cache.tar.gz" ]; then
    echo "using ${SINDRI_GATE_CACHE}/trivy-cache.tar.gz (the pre-seed block already ran once)"
    cp "${SINDRI_GATE_CACHE}/trivy-cache.tar.gz" .
  else
    run_block preseed
    if [ -n "${SINDRI_GATE_CACHE:-}" ]; then mkdir -p "${SINDRI_GATE_CACHE}" && cp trivy-cache.tar.gz "${SINDRI_GATE_CACHE}/"; fi
  fi
)
tar -tzf "${WORK}/preseed/trivy-cache.tar.gz" | grep -q '^\./db/trivy.db$' || fail "the pre-seeded cache has no db/trivy.db"
tar -tzf "${WORK}/preseed/trivy-cache.tar.gz" | grep -q '^\./java-db/trivy-java.db$' || fail "the pre-seeded cache has no java-db/trivy-java.db"
echo "trivy-cache.tar.gz holds db/trivy.db and java-db/trivy-java.db"

# --- 3. air-gapped ----------------------------------------------------------------
step "air-gapped: a network created with --internal"
teardown_stack
docker network create --internal "${AIR_NET}" >/dev/null
docker network connect "${AIR_NET}" trivy-mirror
# The proof that the network has no route out: nothing here can reach ghcr.io.
if docker run --rm --network "${AIR_NET}" --entrypoint wget "${PG_IMAGE}" -q -T 8 -O /dev/null https://ghcr.io/v2/ 2>/dev/null; then
  fail "the internal network reaches ghcr.io"
fi
echo "ghcr.io is unreachable from ${AIR_NET}"

# The jar-containing image: a jar whose pom.properties names log4j-core 2.14.1.
mkdir -p "${WORK}/jar"
python3 -I - "${WORK}/jar" <<'PY'
import sys, zipfile
d = sys.argv[1]
with zipfile.ZipFile(f"{d}/app.jar", "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("META-INF/MANIFEST.MF", "Manifest-Version: 1.0\n")
    z.writestr("META-INF/maven/org.apache.logging.log4j/log4j-core/pom.properties",
               "groupId=org.apache.logging.log4j\nartifactId=log4j-core\nversion=2.14.1\n")
    z.writestr("org/apache/logging/log4j/core/Placeholder.class", b"\xca\xfe\xba\xbe")
open(f"{d}/Dockerfile", "w").write("FROM scratch\nCOPY app.jar /app/app.jar\n")
PY
docker build -q -t localhost:8080/scan/jar:1 "${WORK}/jar" >/dev/null

# Trivy reaches a mirror over HTTPS unless its host is localhost, so the mirror is
# fronted by a TLS listener with a certificate for the name "mirror", and the server
# trusts that certificate through SSL_CERT_FILE, which Sindri passes to Trivy.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=mirror -addext subjectAltName=DNS:mirror \
  -keyout "${WORK}/mirror.key" -out "${WORK}/mirror.crt" >/dev/null 2>&1
cat "${WORK}/mirror.key" "${WORK}/mirror.crt" > "${WORK}/mirror.pem"
chmod 644 "${WORK}/mirror.pem" "${WORK}/mirror.crt"
docker run -d --name trivy-mirror-tls --network "${AIR_NET}" --network-alias mirror \
  -v "${WORK}/mirror.pem:/mirror.pem:ro" "${SOCAT_IMAGE}" \
  OPENSSL-LISTEN:5000,cert=/mirror.pem,verify=0,fork,reuseaddr TCP:trivy-mirror:5000 >/dev/null

write_config "${WORK}/air.yaml" "  enabled: true
  dbRepository: mirror:5000/trivy/trivy-db:2
  javaDbRepository: mirror:5000/trivy/trivy-java-db:1"
start_data "${AIR_NET}"
# The server publishes no port and has no route out. A forwarder on both
# networks hands the host's port 8080 to it, which an --internal network cannot do itself.
start_sindri "${AIR_NET}" "${WORK}/air.yaml" --network-alias "${SERVER}" \
  -v "${WORK}/mirror.crt:/etc/mirror-ca.crt:ro" -e SSL_CERT_FILE=/etc/mirror-ca.crt
start_proxy
wait_ready
echo "the air-gapped server is ready; scanning.dbRepository and javaDbRepository point at mirror:5000"
docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "${SERVER}" | grep -qx "${AIR_NET} " ||
  fail "the server is on a network besides ${AIR_NET}"

echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin >/dev/null
docker push -q localhost:8080/scan/jar:1 >/dev/null
JAR_DIGEST="$(curl -fsSI -u "admin:${SINDRI_TOKEN}" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json' \
  localhost:8080/v2/scan/jar/manifests/1 | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest" {print $2}')"
[ -n "${JAR_DIGEST}" ] || fail "no digest for scan/jar:1"
wait_scan "${JAR_DIGEST}"
api "${SITE}/api/v1/subjects/${JAR_DIGEST}/evidence" |
  jq -e '[.items[] | select(.type == "scan")][0].metadata | .producer == "sindri"' >/dev/null ||
  fail "no Sindri scan of the jar image"
echo "scan of the jar image succeeded with no route to ghcr.io"
api "${SITE}/api/v1/subjects/${JAR_DIGEST}/evidence" | jq -c '[.items[] | select(.type == "scan")][0].metadata | {severity_counts, vulnerabilities}'
# The only registry this server can reach is the mirror, so a database in its cache
# came from there.
for db in db/trivy.db java-db/trivy-java.db; do
  docker cp "${SERVER}:/var/cache/sindri/trivy/${db}" "${WORK}/$(basename "${db}")" >/dev/null 2>&1 ||
    fail "the server's cache has no ${db}"
done
echo "the server's cache holds the vulnerability and Java databases it fetched from the mirror"

DIGEST="${JAR_DIGEST}" run_block staleness | sed "s/^/  /"

# --- 4. pre-seeded cache ------------------------------------------------------------
step "the page: pre-seeded cache with skipDBUpdate, no mirror at all"
docker rm -fv trivy-mirror >/dev/null
teardown_stack
# The page's unpack block, with its volume renamed so a real one is never touched.
block unpack | sed 's/sindri-trivy-cache/sindri-m11-trivy-cache/g' > "${WORK}/block-unpack.sh"
grep -q sindri-m11-trivy-cache "${WORK}/block-unpack.sh" || fail "the unpack block names no sindri-trivy-cache volume"
( cd "${WORK}/preseed" && bash -uo pipefail "${WORK}/block-unpack.sh" >/dev/null )
write_config "${WORK}/seed.yaml" "  enabled: true
  skipDBUpdate: true"
start_data "${AIR_NET}"
start_sindri "${AIR_NET}" "${WORK}/seed.yaml" --network-alias "${SERVER}" -v sindri-m11-trivy-cache:/var/cache/sindri/trivy
start_proxy
wait_ready
docker push -q localhost:8080/scan/jar:1 >/dev/null
wait_scan "${JAR_DIGEST}"
echo "a pre-seeded cache with skipDBUpdate scans the jar image with no mirror and no route out"

step "the page: clean up"
run_block cleanup
echo
echo "M11a scan gate passed: sbom and scan evidence, severity refusals naming CVEs for an image, a wheel and a tarball, a passing promotion, and air-gapped scans of a jar image from a mirror and from a pre-seeded cache"
