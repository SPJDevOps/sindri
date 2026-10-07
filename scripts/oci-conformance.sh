#!/usr/bin/env bash
# Run the upstream OCI distribution-spec conformance suite against a local
# Sindri instance. Creates the conformance namespace via the management API
# first. Since M7 every route needs a credential; the suite sends the token as
# the Basic password, the same way docker does.
#
# Usage:
#   make up && make build
#   ./bin/sindri serve &
#   ./scripts/oci-conformance.sh
#
# Environment overrides:
#   SINDRI_URL          default http://127.0.0.1:8080
#   SINDRI_TOKEN        default sindri-dev-token
#   OCI_NAMESPACE       default conformance/test
#   CONFORMANCE_IMAGE   default ghcr.io/opencontainers/distribution-spec/conformance:latest

set -euo pipefail

SINDRI_URL="${SINDRI_URL:-http://127.0.0.1:8080}"
SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
OCI_NAMESPACE="${OCI_NAMESPACE:-conformance/test}"
CONFORMANCE_IMAGE="${CONFORMANCE_IMAGE:-ghcr.io/opencontainers/distribution-spec/conformance:v1.1.0}"

echo "ensuring hosted OCI repository ${OCI_NAMESPACE} exists"
code="$(curl -sS -o /tmp/sindri-repo.json -w '%{http_code}' \
  -X POST "${SINDRI_URL}/api/v1/repositories" \
  -H "Authorization: Bearer ${SINDRI_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${OCI_NAMESPACE}\",\"type\":\"hosted\",\"protocol\":\"oci\"}")"
if [[ "${code}" != "201" && "${code}" != "409" ]]; then
  echo "failed to create repository: HTTP ${code}" >&2
  cat /tmp/sindri-repo.json >&2 || true
  exit 1
fi

# host.docker.internal reaches the host from Docker Desktop / recent Docker Engine.
OCI_ROOT_URL="${OCI_ROOT_URL:-http://host.docker.internal:8080}"
# If SINDRI_URL points somewhere else, derive a docker-reachable root when possible.
if [[ "${SINDRI_URL}" != "http://127.0.0.1:8080" && "${SINDRI_URL}" != "http://localhost:8080" ]]; then
  OCI_ROOT_URL="${SINDRI_URL}"
fi

echo "running conformance against ${OCI_ROOT_URL} namespace=${OCI_NAMESPACE}"
docker run --rm \
  -e OCI_ROOT_URL="${OCI_ROOT_URL}" \
  -e OCI_NAMESPACE="${OCI_NAMESPACE}" \
  -e OCI_USERNAME=conformance \
  -e OCI_PASSWORD="${SINDRI_TOKEN}" \
  -e OCI_TEST_PUSH=1 \
  -e OCI_TEST_PULL=1 \
  -e OCI_TEST_CONTENT_MANAGEMENT=1 \
  "${CONFORMANCE_IMAGE}"
