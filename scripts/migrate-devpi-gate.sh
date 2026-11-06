#!/usr/bin/env bash
# M10 migration gate: docs/migrate/devpi.md, end to end, against the Compose
# stack and a real devpi.
#
#   make up && make build && ./scripts/migrate-devpi-gate.sh
#
# Needs Docker and network access to pypi.org and files.pythonhosted.org (the
# containers pip-install pinned tools, and the page's public member really
# proxies pypi.org). The images are pinned by digest below.
#
# The commands and configuration are read out of the page, between its
# <!-- migrate:NAME --> markers, not copied into this script, so the page cannot
# drift from what passed. The only edits made to them are replacing the page's
# example hosts (devpi.example.com, sindri.example.com) with the real ones and
# the netrc's token placeholder with the token; the gate adds one line, a
# trusted-host for pip, because the gate's Sindri speaks plain HTTP.
#
# What this isolates, and what it does not. devpi, nginx and the clients run in
# containers on a private Docker network; pip and twine run in a container with
# an empty HOME and a generated pip configuration, so nothing from the machine
# that runs the gate is read. Sindri is a process on the host, and the
# containers reach it, and devpi, through the host's published ports. The
# host's network is not isolated: pip, twine, and Sindri's public `pypi` member
# really read pypi.org.
#
# devpi has no read permission of its own, so reads are locked down the way a
# real deployment does it: devpi-lockdown 2.0.0 with nginx's auth_request. Sindri
# authenticates to it with a username and password credential, and the gate
# fails if it cannot.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
PAGE="${ROOT}/docs/migrate/devpi.md"
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
HOST="localhost:8080"
BASE="http://${HOST}"
# What a container calls the host. --add-host makes the name resolve on Linux as
# it does on Docker Desktop.
CHOST="host.docker.internal"
CBASE="http://${CHOST}:8080"
# python 3.12.16 and nginx 1.28. Bump a digest on purpose, not by accident.
PY_IMAGE="python@sha256:4840d514c583a4d3d8cad3940e68dee6f0f6f3635c12fcd0a3d74c21a5b93a54"
NGINX_IMAGE="nginx@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236"
DEVPI_SERVER_VERSION="6.21.0"
DEVPI_CLIENT_VERSION="7.3.0"
DEVPI_WEB_VERSION="5.2.0"
DEVPI_LOCKDOWN_VERSION="2.0.0"
PIP_VERSION="26.3"
TWINE_VERSION="7.0.0"
BUILD_VERSION="1.3.0"
SETUPTOOLS_VERSION="80.9.0"
KEYRING_VERSION="25.6.0"
KEYRINGS_ALT_VERSION="5.0.2"
NET="sindri-migrate-gate-devpi-net"
DC="sindri-migrate-gate-devpi"
NC="sindri-migrate-gate-devpi-nginx"
CC="sindri-migrate-gate-devpi-client"

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

fail() {
  echo "FAIL: $*" >&2
  # Sindri's own account of what it refused, for the person reading a failure.
  [ -f "${WORK:-/nonexistent}/sindri.log" ] && grep -E '"level":"(WARN|ERROR)"' "${WORK}/sindri.log" | tail -5 >&2
  [ -z "${GATE_HOLD:-}" ] || { echo "holding for ${GATE_HOLD}s" >&2; sleep "${GATE_HOLD}"; }
  exit 1
}

# A listener on 8080 is not ours to kill, and a second server could not bind.
if curl -s -o /dev/null --max-time 2 "${BASE}/readyz"; then
  fail "something is already listening on ${HOST}; stop it and rerun"
fi

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "${p}" 2>/dev/null || true; done
  docker rm -f "${DC}" "${NC}" "${CC}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker rm -f "${DC}" "${NC}" "${CC}" >/dev/null 2>&1 || true
docker network rm "${NET}" >/dev/null 2>&1 || true

PASS_ALICE="alice-gate-password"
export DEVPI_PASSWORD="proxy-gate-password"
PASS_ROOT="root-gate-password"
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }

echo "== 1. start devpi ${DEVPI_SERVER_VERSION} behind nginx, requiring a login to read"
docker network create "${NET}" >/dev/null
docker create --name "${DC}" --network "${NET}" --network-alias devpi "${PY_IMAGE}" sh -c "
  set -e
  python -m venv /opt/dv
  /opt/dv/bin/pip install -q devpi-server==${DEVPI_SERVER_VERSION} devpi-client==${DEVPI_CLIENT_VERSION} \
    devpi-web==${DEVPI_WEB_VERSION} devpi-lockdown==${DEVPI_LOCKDOWN_VERSION} >/dev/null 2>&1
  /opt/dv/bin/devpi-init --serverdir /srv/dv >/dev/null 2>&1
  touch /tmp/installed
  exec /opt/dv/bin/devpi-server --serverdir /srv/dv --host 0.0.0.0 --port 3141 --offline-mode
" >/dev/null
docker start "${DC}" >/dev/null

# nginx's log names who made each request by matching the Authorization header
# against the gate's three known logins, so a log line never holds a credential.
cat > "${WORK}/nginx.conf" <<CONF
map_hash_bucket_size 256;
map \$http_authorization \$who {
    default                 other;
    ""                      anonymous;
"Basic $(b64 "acme:${PASS_ALICE}")"            acme;
    "Basic $(b64 "sindri-proxy:${DEVPI_PASSWORD}")" sindri-proxy;
    "Basic $(b64 "root:${PASS_ROOT}")"              root;
}
log_format who '\$who \$request_method \$uri \$status';
access_log /dev/stdout who;
server {
    listen 8081;
    client_max_body_size 64M;
    # devpi-lockdown: every request is checked by devpi first. It answers 401
    # to anyone who is not signed in, which is what makes reads need a login.
    auth_request /+authcheck;
    location = /+authcheck {
        internal;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Original-URI \$request_uri;
        proxy_set_header X-outside-url \$scheme://\$http_host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_pass http://devpi:3141;
    }
    location / {
        proxy_pass http://devpi:3141;
        proxy_set_header X-outside-url \$scheme://\$http_host;
        proxy_set_header X-Real-IP \$remote_addr;
    }
}
CONF
docker create --name "${NC}" --network "${NET}" -p 127.0.0.1::8081 "${NGINX_IMAGE}" >/dev/null
docker cp "${WORK}/nginx.conf" "${NC}:/etc/nginx/conf.d/default.conf"
docker start "${NC}" >/dev/null
sleep 1
[ "$(docker inspect -f '{{.State.Running}}' "${NC}")" = "true" ] || { docker logs "${NC}" >&2; fail "nginx did not start"; }
NPORT="$(docker port "${NC}" 8081/tcp | head -1 | sed 's/.*://')"
DEVPI_LOCAL="http://127.0.0.1:${NPORT}"
DEVPI_FROM_CLIENT="http://${CHOST}:${NPORT}"
for _ in $(seq 1 300); do
  # nginx answers 401 for an anonymous caller once devpi is up, and 5xx before.
  [ "$(curl -s -o /dev/null -w '%{http_code}' "${DEVPI_LOCAL}/")" = "401" ] &&
    docker exec "${DC}" test -f /tmp/installed 2>/dev/null && break
  sleep 1
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "${DEVPI_LOCAL}/")" = "401" ] || { docker logs "${DC}" >&2; fail "devpi did not start behind nginx"; }
[ "$(curl -s -o /dev/null -w '%{http_code}' "${DEVPI_LOCAL}/root/pypi/+simple/")" = "401" ] || fail "devpi serves an index to an anonymous caller"

# dvi: the devpi command line, in the server container, signed in as root.
dvi() { docker exec -e HOME=/root -w /root "${DC}" /opt/dv/bin/devpi "$@" 2>&1; }
dvi use "http://${NC}:8081" >/dev/null
dvi login root --password="" >/dev/null
dvi user -m root password="${PASS_ROOT}" >/dev/null
dvi login root --password="${PASS_ROOT}" >/dev/null

# page_block NAME [server|client]: the fenced block between the page's markers,
# with the page's example hosts replaced by the real ones. "server" names the
# hosts as Sindri on the host sees them, "client" as a container does.
page_block() {
  local out dhost shost
  if [ "${2:-client}" = server ]; then
    dhost="${DEVPI_LOCAL}"
    shost="${BASE}"
  else
    dhost="${DEVPI_FROM_CLIENT}"
    shost="${CBASE}"
  fi
  out="$(awk -v n="<!-- migrate:$1 -->" -v e="<!-- /migrate:$1 -->" '$0==n{f=1;next}$0==e{f=0}f' "${PAGE}" |
    sed '1d;$d' |
    sed -e "s#https://devpi.example.com#${dhost}#g" -e "s#https://token@sindri.example.com#http://token@${shost#http://}#g" -e "s#https://sindri.example.com#${shost}#g" \
      -e "s#^machine sindri.example.com\$#machine ${CHOST}#" \
      -e "s#YOUR_SINDRI_TOKEN#${SINDRI_TOKEN}#")"
  [ -n "${out}" ] || fail "docs/migrate/devpi.md has no migrate:$1 block"
  if grep -q 'example\.com\|YOUR_' <<<"${out}"; then
    fail "the migrate:$1 block names a host or placeholder the gate does not substitute"
  fi
  printf '%s\n' "${out}"
}
# The keyring commands name the repository by its URL, and the client's URL is
# the one a container uses. (Same substitution as page_block, for a command.)

echo "== 2. the client container: pip ${PIP_VERSION}, twine ${TWINE_VERSION}, build ${BUILD_VERSION}"
mkdir -p "${WORK}/w/home"
chmod 777 "${WORK}/w" "${WORK}/w/home"
docker run -d --name "${CC}" --network "${NET}" --add-host "${CHOST}:host-gateway" \
  -v "${WORK}/w:/w" "${PY_IMAGE}" sleep infinity >/dev/null
# Every pip and twine run starts from an empty home, no pip configuration, and a
# keyring that is a plain file under that home.
BASEENV=(-e HOME=/w/home -e XDG_DATA_HOME=/w/home/.local/share -e XDG_CONFIG_HOME=/w/home/.config
  -e PATH=/opt/c/bin:/opt/k/bin:/usr/local/bin:/usr/bin:/bin -e PIP_CONFIG_FILE=/dev/null -e PIP_NO_INPUT=1
  -e PIP_DISABLE_PIP_VERSION_CHECK=1 -e PYTHON_KEYRING_BACKEND=keyrings.alt.file.PlaintextKeyring)
EXTRA=()
cl() { docker exec ${STDIN:+-i} "${BASEENV[@]}" ${EXTRA[@]+"${EXTRA[@]}"} -w "${WD:-/w}" "${CC}" "$@"; }
docker exec "${CC}" sh -c "python -m venv /opt/c && /opt/c/bin/pip install -q pip==${PIP_VERSION} twine==${TWINE_VERSION} \
  build==${BUILD_VERSION} keyring==${KEYRING_VERSION} keyrings.alt==${KEYRINGS_ALT_VERSION} >/dev/null 2>&1" ||
  fail "installing the client tools failed"
# pip ignores a keyring installed in its own environment, so the command it runs
# comes from a second one, as `pipx install keyring` would leave it.
docker exec "${CC}" sh -c "python -m venv /opt/k && /opt/k/bin/pip install -q keyring==${KEYRING_VERSION} keyrings.alt==${KEYRINGS_ALT_VERSION} >/dev/null 2>&1" ||
  fail "installing the keyring command failed"
[ "$(cl pip --version | awk '{print $2}')" = "${PIP_VERSION}" ] || fail "pip is not ${PIP_VERSION}"
echo "  $(cl twine --version | head -1 | cut -c1-60), pip $(cl pip --version | awk '{print $2}'), python $(cl python -V | awk '{print $2}')"

# build_pkg NAME VERSION: a pure-Python wheel and sdist in /w/dist-NAME-VERSION.
# MARK is in the module, so a file's bytes say which build it is.
build_pkg() {
  local name="$1" version="$2" mod="${1//-/_}" dir="${WORK}/w/src-$1-$2"
  mkdir -p "${dir}/src/${mod}"
  cat > "${dir}/pyproject.toml" <<TOML
[build-system]
requires = ["setuptools==${SETUPTOOLS_VERSION}"]
build-backend = "setuptools.build_meta"

[project]
name = "${name}"
version = "${version}"
TOML
  printf 'MARK = "%s %s"\n' "${name}" "${version}" > "${dir}/src/${mod}/__init__.py"
  WD="/w/src-${name}-${version}" cl python -m build --outdir "/w/dist-${name}-${version}" . >/dev/null 2>&1 ||
    fail "building ${name} ${version} failed"
}

# Uploads straight to devpi, as acme, as devpi's own users do.
dvi use "http://${NC}:8081" >/dev/null
dvi user -c acme password="${PASS_ALICE}" email=acme@gate.test >/dev/null
dvi login acme --password="${PASS_ALICE}" >/dev/null
dvi index -c prod bases= >/dev/null
publish_to_devpi() {
  build_pkg "$1" "$2"
  cl env TWINE_USERNAME=acme TWINE_PASSWORD="${PASS_ALICE}" \
    twine upload --repository-url "${DEVPI_FROM_CLIENT}/acme/prod/" "/w/dist-$1-$2/"* >/dev/null ||
    fail "uploading $1 $2 to devpi failed"
}
publish_to_devpi acme-legacy 1.0.0
publish_to_devpi acme-lib 2.0.0
echo "  acme/prod holds acme-legacy 1.0.0 and acme-lib 2.0.0"

echo "== 3. the page's devpi-user command makes sindri-proxy"
page_block devpi-user > "${WORK}/devpi-user.sh"
docker cp "${WORK}/devpi-user.sh" "${DC}:/root/devpi-user.sh"
docker exec -e HOME=/root -e DEVPI_PASSWORD="${DEVPI_PASSWORD}" -e PATH=/opt/dv/bin:/usr/local/bin:/usr/bin:/bin -w /root "${DC}" \
  bash -e /root/devpi-user.sh >/dev/null || fail "the page's devpi user command failed"
[ "$(curl -s -o /dev/null -w '%{http_code}' -u "sindri-proxy:${DEVPI_PASSWORD}" "${DEVPI_LOCAL}/acme/prod/+simple/")" = "200" ] ||
  fail "sindri-proxy cannot read acme/prod"

echo "== 4. the before: install straight from devpi"
BEFORE_URL="http://acme:${PASS_ALICE}@${CHOST}:${NPORT}/acme/prod/+simple/"
cl mkdir -p /w/before
cl pip download --no-cache-dir --no-deps --index-url "${BEFORE_URL}" --trusted-host "${CHOST}" \
  -d /w/before acme-legacy==1.0.0 >/dev/null || fail "the before download from devpi failed"
BEFORE_SHA="$(cl sha256sum /w/before/acme_legacy-1.0.0-py3-none-any.whl | awk '{print $1}')"
echo "  acme_legacy-1.0.0 from devpi: ${BEFORE_SHA:0:12}"

echo "== 5. configure Sindri from the page's own blocks and start it"
{
  cat <<YML
listen: :8080
storage: \${SINDRI_DATA_DIR}
database: \${SINDRI_DATABASE}
auth: \${SINDRI_TOKEN}
YML
  page_block config server
} > "${WORK}/sindri.yaml"
export SINDRI_DATABASE
SINDRI_DATABASE="$(fresh_db sindri_migrate_devpi)"
SINDRI_DATA_DIR="$(mktemp -d "${WORK}/data.XXXXXX")"
export SINDRI_DATA_DIR
"${BIN}" config check -f "${WORK}/sindri.yaml" >/dev/null || fail "the page's configuration does not pass config check"
"${BIN}" serve --config "${WORK}/sindri.yaml" > "${WORK}/sindri.log" 2>&1 &
PIDS+=("$!")
wait_ready "${BASE}" || { cat "${WORK}/sindri.log" >&2; exit 1; }
echo "  serving the page's configuration"
AUTH=(-H "Authorization: Bearer ${SINDRI_TOKEN}")
status() { curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$1"; }
# files REPO PROJECT: the filenames a repository's JSON index lists, sorted.
files() {
  curl -fsS "${AUTH[@]}" -H 'Accept: application/vnd.pypi.simple.v1+json' "${BASE}/pypi/$1/simple/$2/" |
    python3 -c 'import json,sys; print(" ".join(sorted(f["filename"] for f in json.load(sys.stdin)["files"])))'
}

echo "== 6. the client configuration, from the page"
# A pip configuration per variant, each the page's block. The gate adds
# trusted-host, because this Sindri speaks plain HTTP to a non-loopback name.
mkdir -p "${WORK}/w/home/.config"
page_block pip-conf > "${WORK}/w/pip.conf"
page_block pip-conf-keyring > "${WORK}/w/pip-keyring.conf"
printf 'trusted-host = %s\n' "${CHOST}" | tee -a "${WORK}/w/pip.conf" >> "${WORK}/w/pip-keyring.conf"
page_block netrc > "${WORK}/w/home/.netrc"
chmod 600 "${WORK}/w/home/.netrc"
if grep -q "${DEVPI_LOCAL#http://}\|${DEVPI_FROM_CLIENT#http://}" "${WORK}/w/pip.conf" "${WORK}/w/home/.netrc"; then
  fail "the client configuration names devpi"
fi
pip_sindri() { local EXTRA=(-e PIP_CONFIG_FILE=/w/pip.conf); cl pip "$@"; }

echo "== 7. the after: the same project, with no devpi credential on the client"
# Settle the log before marking where this run's lines start.
dlog() { docker logs "${NC}" 2>&1; }
MARK="$(dlog | wc -l | tr -d ' ')"
page_block install > "${WORK}/w/install.sh"
cl rm -rf /w/after
# `pip download` stands in for `pip install` so the file can be hashed; the
# page's own install command is run next, into a virtual environment.
pip_sindri download -vv --no-cache-dir --no-deps -d /w/after acme-legacy==1.0.0 >"${WORK}/after.log" 2>&1 ||
  { cat "${WORK}/after.log" >&2; fail "pip download through Sindri failed"; }
AFTER_SHA="$(cl sha256sum /w/after/acme_legacy-1.0.0-py3-none-any.whl | awk '{print $1}')"
[ "${AFTER_SHA}" = "${BEFORE_SHA}" ] || fail "the file through Sindri differs from devpi's"
# This greps pip 26.3's -vv log format ("Found link <url> (from <index>)"). If a
# newer pip words it differently, this is the line to fix. The point is that
# pip's own log names Sindri's address for the file and never devpi's.
grep -q "Found link ${CBASE}/pypi/python/packages/acme-legacy/acme_legacy-1.0.0-py3-none-any.whl" "${WORK}/after.log" ||
  { cat "${WORK}/after.log" >&2; fail "pip did not download the file from Sindri's python repository"; }
if grep -q ":${NPORT}" "${WORK}/after.log"; then fail "pip contacted devpi directly after the switch"; fi
cl python -m venv /w/venv-after
EXTRA=(-e PIP_CONFIG_FILE=/w/pip.conf -e PATH=/w/venv-after/bin:/opt/c/bin:/opt/k/bin:/usr/local/bin:/usr/bin:/bin)
cl bash -e /w/install.sh >/dev/null 2>&1 || fail "the page's install command failed"
[ "$(cl /w/venv-after/bin/python -c 'import acme_legacy; print(acme_legacy.MARK)')" = "acme-legacy 1.0.0" ] ||
  fail "the installed acme-legacy is not 1.0.0"
EXTRA=()
# nginx's log shows who read the file from devpi. It lags the request a little.
# This greps the log_format defined above, which the gate owns: "who METHOD uri status".
for _ in $(seq 1 50); do
  dlog | tail -n +"$((MARK + 1))" > "${WORK}/nginx-after.log"
  grep -q "sindri-proxy GET .*acme_legacy-1.0.0-py3-none-any.whl 200" "${WORK}/nginx-after.log" && break
  sleep 0.2
done
grep -q "sindri-proxy GET .*acme_legacy-1.0.0-py3-none-any.whl 200" "${WORK}/nginx-after.log" ||
  { cat "${WORK}/nginx-after.log" >&2; fail "devpi did not log the file read by the sindri-proxy account"; }
if grep -E '^(acme|other|anonymous|root) ' "${WORK}/nginx-after.log" | grep -v '/+authcheck' | grep -q .; then
  grep -E '^(acme|other|anonymous|root) ' "${WORK}/nginx-after.log" >&2
  fail "devpi saw a request that did not come from Sindri's account"
fi
echo "  same SHA-256 through Sindri; pip named only Sindri; devpi logged the read as sindri-proxy"

echo "== 8. a public project still installs: devpi answers 404, the pypi member serves it"
cl rm -rf /w/public
pip_sindri download --no-cache-dir --no-deps -d /w/public six==1.16.0 >/dev/null 2>&1 || fail "six==1.16.0 did not come through the pypi member"
echo "  six 1.16.0 downloaded through python"

echo "== 9. keyring: the page's pip.conf with keyring-provider = subprocess"
page_block keyring-pip > "${WORK}/w/keyring-pip.sh"
printf "%s\n" "${SINDRI_TOKEN}" | STDIN=1 cl bash -e /w/keyring-pip.sh >/dev/null 2>&1 || fail "the page's keyring set command failed"
cl rm -rf /w/after-keyring
# The netrc is moved aside so only the keyring can supply the token.
cl mv /w/home/.netrc /w/home/.netrc.off
EXTRA=(-e PIP_CONFIG_FILE=/w/pip-keyring.conf)
cl pip download --no-cache-dir --no-deps -d /w/after-keyring acme-legacy==1.0.0 >"${WORK}/keyring.log" 2>&1 ||
  { cat "${WORK}/keyring.log" >&2; fail "pip with keyring-provider = subprocess did not authenticate"; }
# With the keyring empty the same command must fail, or it proved nothing.
cl rm -f /w/home/.local/share/python_keyring/keyring_pass.cfg
if cl pip download --no-cache-dir --no-deps -d /w/after-keyring2 acme-legacy==1.0.0 >/dev/null 2>&1; then
  fail "pip authenticated with an empty keyring and no netrc: the keyring step proved nothing"
fi
EXTRA=()
cl mv /w/home/.netrc.off /w/home/.netrc
echo "  pip authenticated from the keyring, and failed without it"

echo "== 10. upload to hosted from the page: .pypirc, keyring, twine"
page_block pypirc > "${WORK}/w/home/.pypirc"
page_block keyring-twine > "${WORK}/w/keyring-twine.sh"
printf "%s\n" "${SINDRI_TOKEN}" | STDIN=1 cl bash -e /w/keyring-twine.sh >/dev/null 2>&1 || fail "the page's keyring set command for twine failed"
# publish_hosted NAME VERSION: build, then run the page's twine command. Its
# `python -m build` line is the build_pkg step; its twine line is run as written.
publish_hosted() {
  build_pkg "$1" "$2"
  page_block publish | grep '^twine ' | sed "s# dist/\*# /w/dist-$1-$2/*#" > "${WORK}/w/publish.sh"
  grep -q 'twine upload --repository sindri' "${WORK}/w/publish.sh" || fail "the page's publish block has no twine upload line"
  cl bash -e /w/publish.sh >/dev/null 2>&1 || fail "the page's twine upload of $1 $2 failed"
}
# This is the order the page warns against, on purpose: acme-lib 1.5.0 is
# uploaded to hosted before devpi's acme-lib 2.0.0 is imported.
publish_hosted acme-lib 1.5.0
[ "$(files internal-py acme-lib)" = "acme_lib-1.5.0-py3-none-any.whl acme_lib-1.5.0.tar.gz" ] || fail "hosted does not list acme-lib 1.5.0"
echo "  acme-lib 1.5.0 uploaded to internal-py"

echo "== 11. the name claim: one upload hides devpi's acme-lib from python"
# `devpi` is the control: a proxy alone has what devpi holds now.
[ "$(files devpi acme-lib)" = "acme_lib-2.0.0-py3-none-any.whl acme_lib-2.0.0.tar.gz" ] || fail "control: the proxy alone does not list devpi's acme-lib 2.0.0"
[ "$(files python acme-lib)" = "acme_lib-1.5.0-py3-none-any.whl acme_lib-1.5.0.tar.gz" ] ||
  fail "acme-lib through python is not exactly the hosted 1.5.0 (devpi's 2.0.0 leaked in)"
code="$(status "${BASE}/pypi/python/packages/acme-lib/acme_lib-2.0.0-py3-none-any.whl")"
[ "${code}" = "404" ] || fail "devpi's acme_lib-2.0.0 through python answered ${code}, want 404"
# The claim is per project: acme-legacy was not touched.
[ "$(files python acme-legacy)" = "acme_legacy-1.0.0-py3-none-any.whl acme_legacy-1.0.0.tar.gz" ] || fail "an upload to acme-lib changed acme-legacy"
cl rm -rf /w/claim
cl python -m venv /w/venv-claim
EXTRA=(-e PIP_CONFIG_FILE=/w/pip.conf)
cl /w/venv-claim/bin/pip install --no-cache-dir acme-lib >/dev/null 2>&1 || fail "installing acme-lib through python failed"
[ "$(cl /w/venv-claim/bin/python -c 'import acme_lib; print(acme_lib.MARK)')" = "acme-lib 1.5.0" ] ||
  fail "pip through python did not install the hosted acme-lib 1.5.0"
echo "  acme-lib: python lists and installs only the hosted 1.5.0; the proxy alone has 2.0.0; acme-legacy is unaffected"

echo "== 12. --extra-index-url: the page's pitfall installs the version from the other index"
page_block extra-index > "${WORK}/w/extra-index.sh"
cl python -m venv /w/venv-extra
# The page's command, run in a clean environment whose pip finds the netrc.
EXTRA=(-e PATH=/w/venv-extra/bin:/opt/c/bin:/opt/k/bin:/usr/local/bin:/usr/bin:/bin)
# pip in the venv is the venv's own; trusted-host comes from the environment.
EXTRA+=(-e PIP_TRUSTED_HOST="${CHOST}")
cl bash -e /w/extra-index.sh >/dev/null 2>&1 || fail "the page's --extra-index-url command failed"
[ "$(cl /w/venv-extra/bin/python -c 'import acme_lib; print(acme_lib.MARK)')" = "acme-lib 2.0.0" ] ||
  fail "the --extra-index-url form did not install the other index's higher version: the page's warning is stale"
EXTRA=()
echo "  --extra-index-url installed acme-lib 2.0.0 although hosted holds 1.5.0; python installed 1.5.0"

echo "== 13. import the history, from the page"
cl rm -rf /w/import
cl mkdir -p /w/import
page_block export > "${WORK}/w/import/export-devpi.py"
page_block export-run > "${WORK}/w/import/export.sh"
page_block import > "${WORK}/w/import/import.sh"
EXTRA=(-e DEVPI_PASSWORD="${DEVPI_PASSWORD}")
WD=/w/import cl bash -e export.sh > "${WORK}/inventory.txt" || fail "the page's export script failed"
sort "${WORK}/inventory.txt" > "${WORK}/inventory.sorted"
cat > "${WORK}/inventory.want" <<'LIST'
acme-legacy acme_legacy-1.0.0-py3-none-any.whl
acme-legacy acme_legacy-1.0.0.tar.gz
acme-lib acme_lib-2.0.0-py3-none-any.whl
acme-lib acme_lib-2.0.0.tar.gz
LIST
diff "${WORK}/inventory.want" "${WORK}/inventory.sorted" >&2 || fail "the export did not list exactly what devpi holds"
EXTRA=(-e SINDRI_TOKEN="${SINDRI_TOKEN}")
WD=/w/import cl bash -e import.sh >"${WORK}/import.log" 2>&1 || { tail -15 "${WORK}/import.log" >&2; fail "the page's import upload failed"; }
EXTRA=(-e SINDRI_TOKEN="${SINDRI_TOKEN}")
# Running it again must not fail: --skip-existing, as the page says.
WD=/w/import cl bash -e import.sh >/dev/null 2>&1 || fail "the page's import upload is not safe to repeat"
EXTRA=()
want="acme_lib-1.5.0-py3-none-any.whl acme_lib-1.5.0.tar.gz acme_lib-2.0.0-py3-none-any.whl acme_lib-2.0.0.tar.gz"
[ "$(files python acme-lib)" = "${want}" ] || fail "after the import python does not list acme-lib 1.5.0 and 2.0.0"
# The imported file is devpi's bytes.
IMPORTED="$(curl -fsS "${AUTH[@]}" "${BASE}/pypi/internal-py/packages/acme-legacy/acme_legacy-1.0.0-py3-none-any.whl" | shasum -a 256 | awk '{print $1}')"
[ "${IMPORTED}" = "${BEFORE_SHA}" ] || fail "the imported acme_legacy-1.0.0 is not devpi's file"
echo "  the inventory matched devpi, the import is repeatable, and acme-lib 2.0.0 is back beside 1.5.0"

echo "== 14. upload a new release after the import"
publish_hosted acme-legacy 1.0.1
cl rm -rf /w/final
cl python -m venv /w/venv-final
EXTRA=(-e PIP_CONFIG_FILE=/w/pip.conf)
cl /w/venv-final/bin/pip install --no-cache-dir acme-legacy >/dev/null 2>&1 || fail "installing acme-legacy through python failed"
[ "$(cl /w/venv-final/bin/python -c 'import acme_legacy; print(acme_legacy.MARK)')" = "acme-legacy 1.0.1" ] ||
  fail "python did not install the newly uploaded acme-legacy 1.0.1"
cl /w/venv-final/bin/pip install --no-cache-dir acme-legacy==1.0.0 >/dev/null 2>&1 || fail "the imported acme-legacy 1.0.0 no longer installs"
EXTRA=()
echo "  acme-legacy 1.0.1 (new) and 1.0.0 (imported) both install through python"

echo "== 15. secrets"
if grep -q "${DEVPI_PASSWORD}" "${WORK}/sindri.log"; then fail "the devpi password is in Sindri's log"; fi
if grep -q "${SINDRI_TOKEN}" "${WORK}/sindri.log" "${WORK}"/*.log; then fail "the Sindri token is in a log"; fi
if docker logs "${NC}" 2>&1 | grep -q "${DEVPI_PASSWORD}\|${PASS_ALICE}"; then fail "a password is in nginx's log"; fi
echo "M10 devpi migration gate passed"
