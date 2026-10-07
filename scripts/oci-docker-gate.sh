#!/usr/bin/env bash
# M3 docker push/pull gate helper.
#
# On Docker Desktop (Mac/Windows) the daemon runs in a VM, so localhost:8080
# inside the daemon is not the host. Tag images as host.docker.internal:8080/...
# and list that host as an insecure registry (Docker Desktop → Settings →
# Docker Engine → insecure-registries).
#
# Prerequisites: make up, make build, sindri serve listening on :8080.
# Set CREATE_REPO=0 when the repository is declared in the configuration.
set -euo pipefail

SINDRI_URL="${SINDRI_URL:-http://127.0.0.1:8080}"
SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
REPO="${REPO:-demo/app}"
TAG="${TAG:-1.0}"
# Override REGISTRY_HOST=localhost when the docker daemon shares the host network.
REGISTRY_HOST="${REGISTRY_HOST:-host.docker.internal:8080}"
IMAGE="${REGISTRY_HOST}/${REPO}:${TAG}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ "${CREATE_REPO:-1}" == "1" ]]; then
  echo "ensuring hosted OCI repository ${REPO}"
  code="$(curl -sS -o /tmp/sindri-demo-repo.json -w '%{http_code}' \
    -X POST "${SINDRI_URL}/api/v1/repositories" \
    -H "Authorization: Bearer ${SINDRI_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"${REPO}\",\"type\":\"hosted\",\"protocol\":\"oci\"}")"
  if [[ "${code}" != "201" && "${code}" != "409" ]]; then
    echo "failed to create repository: HTTP ${code}" >&2
    cat /tmp/sindri-demo-repo.json >&2 || true
    exit 1
  fi
fi

# Since M7 there is no anonymous push or pull (ADR 18). The token is the Basic
# password: the bootstrap token, a machine token, or an identity-provider
# token.
echo "${SINDRI_TOKEN}" | docker login "${REGISTRY_HOST}" -u gate --password-stdin

docker build -t "${IMAGE}" "${ROOT}/testdata/demo"
if ! docker push "${IMAGE}"; then
  cat >&2 <<EOF
docker push failed. On Docker Desktop, add to Docker Engine JSON:

  "insecure-registries": ["${REGISTRY_HOST%%/*}"]

Then retry. Or set REGISTRY_HOST=localhost:8080 on Linux with host networking.
EOF
  exit 1
fi
docker rmi "${IMAGE}"
docker pull "${IMAGE}"
echo "docker push/pull round-trip ok for ${IMAGE}"
