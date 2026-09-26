#!/usr/bin/env bash
# M4 twine upload / pip install gate helper.
#
# Prerequisites: make up, make build, sindri serve listening on :8080,
# and a Python with twine, build, and pip available (python3 -m pip install
# twine build).
set -euo pipefail

SINDRI_URL="${SINDRI_URL:-http://127.0.0.1:8080}"
SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
REPO="${REPO:-internal}"
PKG_DIR="${PKG_DIR:-}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -z "${PKG_DIR}" ]]; then
  PKG_DIR="${ROOT}/testdata/pypi/demo-package"
fi

echo "ensuring hosted PyPI repository ${REPO}"
code="$(curl -sS -o /tmp/sindri-pypi-repo.json -w '%{http_code}' \
  -X POST "${SINDRI_URL}/api/v1/repositories" \
  -H "Authorization: Bearer ${SINDRI_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${REPO}\",\"type\":\"hosted\",\"protocol\":\"pypi\"}")"
if [[ "${code}" != "201" && "${code}" != "409" ]]; then
  echo "failed to create repository: HTTP ${code}" >&2
  cat /tmp/sindri-pypi-repo.json >&2 || true
  exit 1
fi

# A published file is immutable (ADR 12) and a wheel is not byte-reproducible,
# so each run stamps its own version instead of colliding with the last run's
# upload. Override with SINDRI_GATE_VERSION to publish a specific version.
VERSION="${SINDRI_GATE_VERSION:-1.0.0.post$(date -u +%Y%m%d%H%M%S)}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
cp -R "${PKG_DIR}/." "${WORKDIR}/"
stamp_version() {
  # Rewritten with python rather than sed: BSD and GNU sed disagree about -i.
  python3 - "$1" <<'PY'
import pathlib, re, sys

version = sys.argv[1]
for path, key in (("pyproject.toml", "version"), ("src/demo_package/__init__.py", "__version__")):
    p = pathlib.Path(path)
    p.write_text(re.sub(rf"(?m)^{key} = .*$", f'{key} = "{version}"', p.read_text()))
PY
}
(
  cd "${WORKDIR}"
  rm -rf dist build *.egg-info
  stamp_version "${VERSION}"
  python3 -m build --wheel --outdir dist
)

echo "twine upload to ${SINDRI_URL}/pypi/${REPO}/upload"
python3 -m twine upload \
  --repository-url "${SINDRI_URL}/pypi/${REPO}/upload" \
  -u token -p "${SINDRI_TOKEN}" \
  "${WORKDIR}/dist/"*

VENV="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}" "${VENV}"' EXIT
python3 -m venv "${VENV}"
"${VENV}/bin/pip" install --upgrade pip
HOST="${SINDRI_URL#*://}"
HOST="${HOST%%/*}"
echo "pip install demo-package==${VERSION} from ${SINDRI_URL}/pypi/${REPO}/simple/"
"${VENV}/bin/pip" install \
  --index-url "${SINDRI_URL}/pypi/${REPO}/simple/" \
  --trusted-host "${HOST}" \
  "demo-package==${VERSION}"

installed="$("${VENV}/bin/python" -c 'import demo_package; print(demo_package.__version__)')"
if [[ "${installed}" != "${VERSION}" ]]; then
  echo "installed ${installed}, expected ${VERSION}" >&2
  exit 1
fi

echo "PEP 691 JSON index"
curl -fsS -H 'Accept: application/vnd.pypi.simple.v1+json' \
  "${SINDRI_URL}/pypi/${REPO}/simple/demo-package/" | python3 -m json.tool >/dev/null

# A published file never changes under a consumer who pinned its hash.
echo "re-upload of the same filename with different bytes is refused"
(
  cd "${WORKDIR}"
  printf '\n# rebuilt at %s\n' "$(date -u +%s)" >> src/demo_package/__init__.py
  rm -rf dist build *.egg-info
  python3 -m build --wheel --outdir dist
)
WHEEL="$(ls "${WORKDIR}"/dist/*.whl | head -1)"
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  -X POST "${SINDRI_URL}/pypi/${REPO}/upload" \
  -u "token:${SINDRI_TOKEN}" \
  -F ':action=file_upload' \
  -F 'name=demo-package' \
  -F "version=${VERSION}" \
  -F "content=@${WHEEL}")"
if [[ "${code}" != "409" ]]; then
  echo "re-upload with different bytes returned HTTP ${code}, expected 409" >&2
  exit 1
fi

echo "twine/pip round-trip ok for demo-package ${VERSION} on ${REPO}"
