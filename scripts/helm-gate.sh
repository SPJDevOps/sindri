#!/usr/bin/env bash
# Helm gate: the chart installs, serves, and upgrades in kind, by the commands
# docs/deploy/helm.md writes.
#
#   ./scripts/helm-gate.sh
#
# Needs Docker, kind, kubectl, and helm. The values and the commands are read
# out of the page, between its <!-- helm:... --> markers, so the page cannot
# drift from what passed.
#
# Two things differ from the page, because the chart and the image are not in a
# registry until a release: the chart reference becomes ./charts/sindri, and
# the image is built from this checkout and loaded into kind. Postgres and MinIO
# are throwaway deployments here, standing in for the ones an operator brings.
#
# crane runs from a pinned image on kind's Docker network and reaches Sindri on
# a NodePort. An upgrade must recreate the pod and still serve every blob of the
# image pushed before it.
#
# SINDRI_GATE_IMAGE tests an image that is already built.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOC="${ROOT}/docs/deploy/helm.md"
CHART="${ROOT}/charts/sindri"
CLUSTER="sindri-helm-gate"
NODE="${CLUSTER}-control-plane"
POSTGRES_IMAGE="postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5"
# The same pin as internal/testsupport; see ADR 11.
MINIO_IMAGE="bitnamilegacy/minio:2025.7.23-debian-12-r5"
CRANE_IMAGE="gcr.io/go-containerregistry/crane@sha256:ce0d9cffa63d62c547b7df1cad9875fe90e8f7e2413870f0b50ca03839f56901"
SOURCE_IMAGE="alpine:3.20"
IMAGE="${SINDRI_GATE_IMAGE:-}"
WORK="$(mktemp -d)"

cleanup() {
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
cleanup
mkdir -p "${WORK}"

fail() { echo "helm gate: $*" >&2; exit 1; }
for tool in docker kind kubectl helm; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

# block NAME: the fenced block between <!-- helm:NAME --> and its closing marker.
block() {
  awk -v begin="<!-- helm:$1 -->" -v end="<!-- /helm:$1 -->" \
    '$0==begin{f=1;next}$0==end{f=0}f' "${DOC}" | sed '1d;$d'
}

echo "== extracting the page's values and commands"
cd "${WORK}"
block values > values.yaml
block ingress > ingress.yaml
for name in install upgrade; do
  block "${name}" > "${name}.sh"
  grep -q 'oci://ghcr.io/spjdevops/charts/sindri --version ' "${name}.sh" \
    || fail "the ${name} block does not install the published chart"
done
for f in values.yaml ingress.yaml; do test -s "${f}" || fail "${f} is empty"; done

# local_chart TAG: the published chart reference becomes this checkout's chart.
local_chart() {
  sed -E -i.bak "s#oci://ghcr.io/spjdevops/charts/sindri --version [^ ]+#${CHART} --set image.repository=sindri --set image.tag=$1 --set image.pullPolicy=Never#" "$2"
}
local_chart gate install.sh
local_chart gate-upgraded upgrade.sh

echo "== the chart refuses what it must refuse"
refuses() { # WHAT PATTERN ARGS...
  local what="$1" pattern="$2" out
  shift 2
  if out="$(helm template s "${CHART}" "$@" 2>&1)"; then fail "${what} rendered"; fi
  grep -q -- "${pattern}" <<<"${out}" || fail "${what} failed for another reason: ${out}"
}
refuses "no configuration" "config.database is required"
refuses "no token" "config.auth.static_token is required" \
  --set config.database=x --set config.storage=s3://b
refuses "the filesystem backend with no volume" "persistence.enabled must be true" \
  --set config.database=x --set config.storage=/var/lib/sindri --set config.auth.static_token=y
refuses "a replica count" "replicaCount" --values values.yaml --set replicaCount=2

echo "== the Ingress publishes no operational path"
routed="$(helm template s "${CHART}" --values values.yaml --values ingress.yaml --show-only templates/ingress.yaml)"
grep -q 'path: /v2' <<<"${routed}" || fail "the Ingress does not route /v2"
if grep -E 'path: /(metrics|healthz|readyz)' <<<"${routed}"; then fail "the Ingress publishes an operational path"; fi

if [[ -z "${IMAGE}" ]]; then
  echo "== building the Sindri image from this checkout"
  docker build -q -t sindri:gate --build-arg VERSION=helm-gate "${ROOT}" >/dev/null
  IMAGE=sindri:gate
fi
docker tag "${IMAGE}" sindri:gate
docker tag "${IMAGE}" sindri:gate-upgraded

echo "== creating the kind cluster"
kind create cluster --name "${CLUSTER}" --wait 120s >/dev/null
export KUBECONFIG="${WORK}/kubeconfig"
kind get kubeconfig --name "${CLUSTER}" > "${KUBECONFIG}"
kind load docker-image --name "${CLUSTER}" sindri:gate sindri:gate-upgraded >/dev/null

echo "== starting throwaway Postgres and MinIO"
PG_PASSWORD="$(openssl rand -hex 16)"
S3_SECRET="$(openssl rand -hex 16)"
kubectl create namespace deps >/dev/null
kubectl -n deps apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: ${POSTGRES_IMAGE}
          env:
            - {name: POSTGRES_USER, value: sindri}
            - {name: POSTGRES_PASSWORD, value: ${PG_PASSWORD}}
            - {name: POSTGRES_DB, value: sindri}
          ports: [{containerPort: 5432}]
          readinessProbe:
            exec: {command: [pg_isready, -U, sindri, -d, sindri]}
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio}
spec:
  selector: {matchLabels: {app: minio}}
  template:
    metadata: {labels: {app: minio}}
    spec:
      containers:
        - name: minio
          image: ${MINIO_IMAGE}
          env:
            - {name: MINIO_ROOT_USER, value: gate-access-key}
            - {name: MINIO_ROOT_PASSWORD, value: ${S3_SECRET}}
            - {name: MINIO_DEFAULT_BUCKETS, value: sindri-blobs}
          ports: [{containerPort: 9000}]
          readinessProbe:
            httpGet: {path: /minio/health/ready, port: 9000}
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: {name: minio}
spec:
  selector: {app: minio}
  ports: [{port: 9000}]
YAML
kubectl -n deps rollout status deployment/postgres --timeout=300s >/dev/null
kubectl -n deps rollout status deployment/minio --timeout=300s >/dev/null

export DATABASE_URL="postgres://sindri:${PG_PASSWORD}@postgres.deps.svc:5432/sindri?sslmode=disable"
export S3_ENDPOINT="http://minio.deps.svc:9000"
export S3_ACCESS_KEY_ID="gate-access-key"
export S3_SECRET_ACCESS_KEY="${S3_SECRET}"
SINDRI_TOKEN="$(openssl rand -hex 32)"
export SINDRI_TOKEN

echo "== installing, by the page's commands"
bash -euo pipefail install.sh

kubectl -n sindri expose deployment sindri --type=NodePort --name=gate --port=8080 >/dev/null
PORT="$(kubectl -n sindri get service gate -o jsonpath='{.spec.ports[0].nodePort}')"
TARGET="${NODE}:${PORT}/gate/alpine:1"

mkdir -p "${WORK}/crane"
chmod 777 "${WORK}/crane"
crane() {
  docker run --rm -i --network kind -e DOCKER_CONFIG=/cfg -v "${WORK}/crane:/cfg" \
    "${CRANE_IMAGE}" --insecure "$@"
}
printf '%s' "${SINDRI_TOKEN}" | crane auth login "${NODE}:${PORT}" -u admin --password-stdin

# validate_pull: read every layer of the two real images back, by digest.
# `crane validate` on the whole index always fails on this image, also straight
# from Docker Hub: its attestation manifests have in-toto JSON layers, and crane
# expects gzip.
validate_pull() {
  local platform digest
  for platform in linux/amd64 linux/arm64; do
    digest="$(crane --platform "${platform}" digest "${TARGET}")"
    crane validate --remote "${TARGET%:1}@${digest}"
  done
}

echo "== pushing and pulling an image through the release"
crane copy "${SOURCE_IMAGE}" "${TARGET}"
source_digest="$(crane digest "${SOURCE_IMAGE}")"
before="$(crane digest "${TARGET}")"
test "${before}" = "${source_digest}" || fail "pushed ${source_digest}, but Sindri serves ${before}"
validate_pull

pod_before="$(kubectl -n sindri get pods -l app.kubernetes.io/name=sindri -o jsonpath='{.items[0].metadata.uid}')"

echo "== upgrading, by the page's commands"
bash -euo pipefail upgrade.sh

pods="$(kubectl -n sindri get pods -l app.kubernetes.io/name=sindri -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}')"
test "$(wc -l <<<"${pods}" | tr -d ' ')" = 1 || fail "expected one pod after the upgrade, got: ${pods}"
test "${pods}" != "${pod_before}" || fail "the upgrade did not replace the pod"
running="$(kubectl -n sindri get deployment sindri -o jsonpath='{.spec.template.spec.containers[0].image}')"
test "${running}" = "sindri:gate-upgraded" || fail "the upgrade runs ${running}"

echo "== the image survived the upgrade"
after="$(crane digest "${TARGET}")"
test "${before}" = "${after}" || fail "digest changed across the upgrade: ${before} then ${after}"
validate_pull

echo "Helm gate passed: installed, pushed and pulled ${before}, upgraded, and served it again"
