#!/usr/bin/env bash
# M9 gate: npm, pnpm and yarn against hosted, proxy and virtual npm
# repositories, end to end, against the Compose stack.
#
#   make up && make build && ./scripts/npm-gate.sh
#
# Needs node and npm on PATH, and network access to registry.npmjs.org: the
# proxy steps really go there. pnpm 10, yarn 1.22 and yarn 4 are installed from
# npmjs into a temp prefix BEFORE any .npmrc points at Sindri, so nothing is
# installed globally and no user configuration is read or written.
#
# The server runs testdata/config/sindri.yaml (internal-js, npm, javascript) on
# :8080, because that file's publicUrl is http://localhost:8080 and the
# packuments carry that host. Registry URLs here therefore say localhost, not
# 127.0.0.1.
#
# Credentials use PATH-level .npmrc keys, //localhost:8080/npm/<repo>/:_authToken.
# Host-level keys gave ENEEDAUTH on publish and yarn 1 sent no token at all.
# The token lines live in the user-level .npmrc (pnpm will not expand an env
# reference in a project one) and the registry lines in each project's. README.md
# shows these lines, and the gate fails if it stops containing them.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
HOST="localhost:8080"
BASE="http://${HOST}"
# Exact versions: the yarn 4 assertions below depend on its age-gate behaviour,
# which a newer release may change. Bump them on purpose, not by accident.
PGVER="${PNPM_VERSION:-10.34.9}"
Y1VER="${YARN1_VERSION:-1.22.22}"
Y4VER="${YARN4_VERSION:-4.18.1}"
VERSION="1.0.0"

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

fail() { echo "FAIL: $*" >&2; exit 1; }

# A listener on 8080 is not ours to kill, and a second server could not bind.
if curl -s -o /dev/null --max-time 2 "${BASE}/readyz"; then
  fail "something is already listening on ${HOST}; stop it and rerun"
fi

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "${p}" 2>/dev/null || true; done
  rm -rf "${WORK}"
}
trap cleanup EXIT
# Make Ctrl-C and TERM run cleanup too. It stops only the PIDs started here.
trap 'exit 130' INT
trap 'exit 143' TERM

# Isolate npm from the user's configuration and cache for the whole run.
export npm_config_userconfig="${WORK}/user.npmrc"
export npm_config_globalconfig="${WORK}/global.npmrc"
export npm_config_cache="${WORK}/npm-cache"
export npm_config_update_notifier=false
export npm_config_fund=false
export npm_config_loglevel=warn
export npm_config_audit=false
: > "${npm_config_userconfig}"
: > "${npm_config_globalconfig}"
export HOME="${WORK}/home"
export XDG_CACHE_HOME="${WORK}/xdg-cache"
export XDG_DATA_HOME="${WORK}/xdg-data"
export XDG_CONFIG_HOME="${WORK}/xdg-config"
export COREPACK_ENABLE_AUTO_PIN=0
mkdir -p "${HOME}"

echo "== tools: pnpm ${PGVER}, yarn ${Y1VER}, yarn ${Y4VER} from npmjs into ${WORK}/tools"
mkdir -p "${WORK}/tools/pnpm" "${WORK}/tools/yarn1" "${WORK}/tools/yarn4"
npm install --prefix "${WORK}/tools/pnpm" --no-package-lock "pnpm@${PGVER}" >/dev/null
npm install --prefix "${WORK}/tools/yarn1" --no-package-lock "yarn@${Y1VER}" >/dev/null
npm install --prefix "${WORK}/tools/yarn4" --no-package-lock "@yarnpkg/cli-dist@${Y4VER}" >/dev/null
PNPM="${WORK}/tools/pnpm/node_modules/.bin/pnpm"
YARN1="${WORK}/tools/yarn1/node_modules/.bin/yarn"
YARN4=(node "${WORK}/tools/yarn4/node_modules/@yarnpkg/cli-dist/bin/yarn.js")
echo "node $(node -v), npm $(npm -v), pnpm $("${PNPM}" -v), yarn1 $("${YARN1}" -v), yarn4 $("${YARN4[@]}" -v)"

echo "== serve testdata/config/sindri.yaml on a fresh database and data directory"
export SINDRI_DATABASE
SINDRI_DATABASE="$(fresh_db sindri_m9_npm)"
SINDRI_DATA_DIR="$(mktemp -d "${WORK}/data.XXXXXX")"
export SINDRI_DATA_DIR
"${BIN}" serve --config testdata/config/sindri.yaml > "${WORK}/sindri.log" 2>&1 &
PIDS+=("$!")
wait_ready "${BASE}" || { cat "${WORK}/sindri.log" >&2; exit 1; }

AUTH=(-H "Authorization: Bearer ${SINDRI_TOKEN}")
# status URL: the HTTP status of an authenticated GET.
status() { curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$1"; }
# versions URL: the version keys of a packument, sorted, one per line.
versions() {
  curl -fsS "${AUTH[@]}" "$1" |
    python3 -c 'import json,sys; print("\n".join(sorted(json.load(sys.stdin)["versions"])))'
}

# registry_line REPO and token_line REPO: the lines README.md shows. The token is
# npm's own ${SINDRI_TOKEN} reference, so no file here holds the secret and the
# gate exercises the same expansion a reader gets. The registry goes in the
# project .npmrc; the token goes in the user-level one, because pnpm refuses to
# expand environment variables in a credential from a project .npmrc.
registry_line() { printf 'registry=%s/npm/%s/\n' "${BASE}" "$1"; }
token_line() {
  # shellcheck disable=SC2016  # the ${...} is for npm to expand, not the shell
  printf '//%s/npm/%s/:_authToken=${SINDRI_TOKEN}\n' "${HOST}" "$1"
}

# write_npmrc DIR REPO: the project .npmrc, which names the registry only.
write_npmrc() { registry_line "$2" > "$1/.npmrc"; }

# The user-level .npmrc, with a path-level token for each repository the
# clients below use. HOME is the temp directory, so yarn and pnpm find it too.
for repo in internal-js javascript; do token_line "${repo}"; done > "${npm_config_userconfig}"
cp "${npm_config_userconfig}" "${HOME}/.npmrc"

# The README must show what this gate runs. Each line is looked up whole, so a
# README that drifts from the gate fails here instead of misleading a reader.
readme_has() { grep -Fxq -- "$1" "${ROOT}/README.md" || fail "README.md does not contain the line: $1"; }
for repo in internal-js javascript; do
  readme_has "$(registry_line "${repo}")"
  readme_has "$(token_line "${repo}")"
done
# The commands are run below with the same name and flags, plus gate-only paths
# (store, cache and lockfile-version options) on the pnpm and yarn lines.
for cmd in 'npm publish --access public' 'npm install' 'pnpm import' \
  'pnpm install --frozen-lockfile' 'yarn import' 'yarn install --frozen-lockfile'; do
  readme_has "${cmd}"
done
# The yarn 4 settings README.md shows. They are written to .yarnrc.yml below.
YARN4_README_LINES=(
  'npmRegistryServer: "http://localhost:8080/npm/javascript/"'
  'unsafeHttpWhitelist:'
  '  - localhost'
  'npmAlwaysAuth: true'
  'npmAuthToken: "${SINDRI_TOKEN}"'
)
# The scope exemption README.md shows for an internal scope installed within a day.
YARN4_PREAPPROVED_LINES=(
  'npmPreapprovedPackages:'
  '  - "@acme/*"'
)
for line in "${YARN4_README_LINES[@]}" "${YARN4_PREAPPROVED_LINES[@]}"; do readme_has "${line}"; done

# tree_hash DIR: one hash over the relative path and content of every file in
# the installed package, so a changed, missing or extra file changes it.
tree_hash() {
  (cd "$1" && find . -type f | LC_ALL=C sort | while read -r f; do
    printf '%s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$f"
  done | shasum -a 256 | cut -d' ' -f1)
}

# assert_demo DIR LABEL: @acme/demo is in DIR/node_modules at the published version.
assert_demo() {
  local pkg="$1/node_modules/@acme/demo/package.json"
  [ -f "${pkg}" ] || fail "$2: @acme/demo did not land in node_modules"
  [ "$(node -p "require('${pkg}').version")" = "${VERSION}" ] ||
    fail "$2: @acme/demo is not version ${VERSION}"
  echo "  $2: @acme/demo@${VERSION} installed"
}

echo "== 1. npm publish @acme/demo to internal-js"
PUB="${WORK}/publish"
mkdir -p "${PUB}/lib"
cat > "${PUB}/package.json" <<JSON
{ "name": "@acme/demo", "version": "${VERSION}", "main": "index.js", "license": "MIT" }
JSON
echo "module.exports = require('./lib/answer.js') + 1;" > "${PUB}/index.js"
echo "module.exports = 41;" > "${PUB}/lib/answer.js"
printf 'sindri m9 gate fixture\n' > "${PUB}/README.md"
write_npmrc "${PUB}" internal-js
(cd "${PUB}" && npm publish --access public >/dev/null)
[ "$(versions "${BASE}/npm/internal-js/@acme%2fdemo")" = "${VERSION}" ] ||
  fail "internal-js packument does not list exactly @acme/demo@${VERSION}"
echo "  published and listed in internal-js"

echo "== 2. npm install it through javascript in a clean directory"
NPMDIR="${WORK}/npm-client"
mkdir -p "${NPMDIR}"
printf '{ "name": "npm-client", "version": "0.0.0", "private": true, "dependencies": { "@acme/demo": "%s" } }\n' \
  "${VERSION}" > "${NPMDIR}/package.json"
write_npmrc "${NPMDIR}" javascript
(cd "${NPMDIR}" && npm install >/dev/null)
assert_demo "${NPMDIR}" npm
grep -q "${BASE}/npm/javascript/" "${NPMDIR}/package-lock.json" ||
  fail "package-lock.json does not resolve through ${BASE}/npm/javascript/"

echo "== 3. pnpm import from that package-lock.json, then install --frozen-lockfile"
PNPMDIR="${WORK}/pnpm-client"
mkdir -p "${PNPMDIR}"
cp "${NPMDIR}/package.json" "${NPMDIR}/package-lock.json" "${NPMDIR}/.npmrc" "${PNPMDIR}/"
(cd "${PNPMDIR}" && "${PNPM}" import >/dev/null && "${PNPM}" install --frozen-lockfile --store-dir "${WORK}/pnpm-store" >/dev/null)
assert_demo "${PNPMDIR}" pnpm

echo "== 4. yarn 1: import, then install --frozen-lockfile"
Y1DIR="${WORK}/yarn1-client"
mkdir -p "${Y1DIR}"
# yarn import reads the installed tree, so it takes the npm client's node_modules.
cp -R "${NPMDIR}/." "${Y1DIR}/"
# yarn 1 reads only the "dependencies" tree of package-lock.json, which npm 7+
# omits at lockfileVersion 3 ("source file(s) corrupted"). Same resolution,
# lockfileVersion 2 spelling: that is a client limitation, not Sindri's.
(cd "${Y1DIR}" && npm install --package-lock-only --lockfile-version=2 >/dev/null)
(cd "${Y1DIR}" && "${YARN1}" import --cache-folder "${WORK}/yarn1-cache" >/dev/null)
[ -f "${Y1DIR}/yarn.lock" ] || fail "yarn import did not write yarn.lock"
rm -rf "${Y1DIR}/node_modules" "${Y1DIR}/package-lock.json"
(cd "${Y1DIR}" && "${YARN1}" install --frozen-lockfile --cache-folder "${WORK}/yarn1-cache" >/dev/null) ||
  fail "yarn 1 install failed (rerun it by hand: product finding if it is a 401 or a wrong dist.tarball)"
assert_demo "${Y1DIR}" yarn1

echo "== 5. yarn 4: install with .yarnrc.yml, http allowed for localhost"
# yarn 4 cannot import an npm lockfile the way yarn 1 does, so it resolves from
# package.json instead. It is the same package through the same repository, and
# the same assertion applies.
Y4DIR="${WORK}/yarn4-client"
mkdir -p "${Y4DIR}"
cp "${NPMDIR}/package.json" "${Y4DIR}/"
: > "${Y4DIR}/yarn.lock"
{
  cat <<YML
nodeLinker: node-modules
enableTelemetry: false
enableGlobalCache: false
cacheFolder: ${WORK}/yarn4-cache
YML
  printf '%s\n' "${YARN4_README_LINES[@]}"
} > "${Y4DIR}/.yarnrc.yml"
Y4_BASE_RC="$(cat "${Y4DIR}/.yarnrc.yml")"
# yarn 4 holds back a version younger than npmMinimalAgeGate (1440 minutes by
# default), judging by the packument's `time` map. Sindri serves each version's
# real publish time, so the gate works as it does against npmjs, with no setting.
#
# @acme/demo was published seconds ago, so yarn 4 must refuse it. Looked for by
# its code, YN0016.
if (cd "${Y4DIR}" && YARN_IGNORE_PATH=1 "${YARN4[@]}" install >"${WORK}/yarn4-default.log" 2>&1); then
  fail "yarn 4 installed a version published seconds ago: its age gate is not being applied"
fi
grep -q YN0016 "${WORK}/yarn4-default.log" ||
  { cat "${WORK}/yarn4-default.log" >&2; fail "yarn 4 failed on a new version, but not with the expected YN0016 quarantine"; }
echo "  yarn 4 holds back a version published seconds ago (YN0016), on defaults"
# The exemption README.md documents: the same fresh version installs once its
# scope is pre-approved, so an internal package can be used the day it is published.
printf '%s\n' "${Y4_BASE_RC}" "${YARN4_PREAPPROVED_LINES[@]}" > "${Y4DIR}/.yarnrc.yml"
rm -rf "${XDG_DATA_HOME}/yarn/berry/metadata"
(cd "${Y4DIR}" && YARN_IGNORE_PATH=1 "${YARN4[@]}" install >"${WORK}/yarn4-preapproved.log" 2>&1) ||
  { cat "${WORK}/yarn4-preapproved.log" >&2; fail "yarn 4 refused a fresh @acme/demo even with npmPreapprovedPackages: [\"@acme/*\"]"; }
assert_demo "${Y4DIR}" "yarn4 with npmPreapprovedPackages"
printf '%s\n' "${Y4_BASE_RC}" > "${Y4DIR}/.yarnrc.yml"
# yarn keeps the packument it fetched, so the refused one would be reused below.
rm -rf "${Y4DIR}/node_modules" "${WORK}/yarn4-cache" "${XDG_DATA_HOME}/yarn/berry/metadata"
: > "${Y4DIR}/yarn.lock"
# Back-date the publish time two days. Nothing else changes, so the install
# below is the same package as the refused one, only older than the gate.
docker exec -e PGOPTIONS='--client-min-messages=warning' "${PG_CONTAINER}" psql -q -v ON_ERROR_STOP=1 -U sindri -d sindri_m9_npm \
  -c "UPDATE versions SET published_at = now() - interval '2 days' WHERE version = '${VERSION}'" >/dev/null
(cd "${Y4DIR}" && YARN_IGNORE_PATH=1 "${YARN4[@]}" install >"${WORK}/yarn4.log" 2>&1) ||
  { cat "${WORK}/yarn4.log" >&2; fail "yarn 4 refused a version older than its age gate (or a 401, or a wrong dist.tarball: a product finding)"; }
assert_demo "${Y4DIR}" yarn4

echo "== 6. every client installed identical bytes"
want="$(tree_hash "${NPMDIR}/node_modules/@acme/demo")"
for c in pnpm-client yarn1-client yarn4-client; do
  got="$(tree_hash "${WORK}/${c}/node_modules/@acme/demo")"
  [ "${got}" = "${want}" ] || fail "${c} installed different bytes (${got} != npm ${want})"
done
# And they are the bytes that were published: the fixture is the reference.
for f in index.js lib/answer.js README.md; do
  cmp -s "${PUB}/${f}" "${NPMDIR}/node_modules/@acme/demo/${f}" ||
    fail "installed ${f} differs from the published file"
done
echo "  npm, pnpm, yarn1 and yarn4 agree: ${want}"

echo "== 7. a proxy-only package (is-number) installs through javascript"
PROXYDIR="${WORK}/proxy-client"
mkdir -p "${PROXYDIR}"
echo '{ "name": "proxy-client", "version": "0.0.0", "private": true, "dependencies": { "is-number": "7.0.0" } }' > "${PROXYDIR}/package.json"
write_npmrc "${PROXYDIR}" javascript
(cd "${PROXYDIR}" && npm install >/dev/null)
[ "$(node -p "require('${PROXYDIR}/node_modules/is-number/package.json').version")" = "7.0.0" ] ||
  fail "is-number@7.0.0 did not install through javascript"
node -e "process.exit(require('${PROXYDIR}/node_modules/is-number')(5) === true ? 0 : 1)" ||
  fail "installed is-number does not work"
echo "  is-number@7.0.0 installed from the proxy"

# A proxied package is old, and its time comes from npmjs. yarn 4 on defaults.
PROXY4="${WORK}/yarn4-proxy"
mkdir -p "${PROXY4}"
echo '{ "name": "proxy-client4", "version": "0.0.0", "private": true, "dependencies": { "is-number": "7.0.0" } }' > "${PROXY4}/package.json"
: > "${PROXY4}/yarn.lock"
printf '%s\n' "${Y4_BASE_RC}" > "${PROXY4}/.yarnrc.yml"
(cd "${PROXY4}" && YARN_IGNORE_PATH=1 "${YARN4[@]}" install >"${WORK}/yarn4-proxy.log" 2>&1) ||
  { cat "${WORK}/yarn4-proxy.log" >&2; fail "yarn 4 on defaults could not install is-number@7.0.0 through the proxy"; }
[ "$(node -p "require('${PROXY4}/node_modules/is-number/package.json').version")" = "7.0.0" ] ||
  fail "yarn 4 did not install is-number@7.0.0 through javascript"
echo "  yarn 4 installed is-number@7.0.0 from the proxy on defaults"

echo "== 8. shadow guard: a hosted name or scope is never served from the proxy"
# @acme/demo answers 404 on npmjs, so it proves nothing alone; these names exist
# there. proxy-first lists the proxy BEFORE the hosted member, so only the
# name-claim rule can keep the hosted package in front.
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"name":"proxy-first","type":"virtual","protocol":"npm","members":["npm","internal-js"],"read":["authenticated"]}' \
  "${BASE}/api/v1/repositories")"
[ "${code}" = "201" ] || fail "creating proxy-first answered ${code}, want 201"

# Hosted is-number@999.0.0-gate.
SHADOW="${WORK}/shadow"
mkdir -p "${SHADOW}"
echo '{ "name": "is-number", "version": "999.0.0-gate", "main": "index.js", "license": "MIT" }' > "${SHADOW}/package.json"
echo "module.exports = function () { return 'hosted'; };" > "${SHADOW}/index.js"
write_npmrc "${SHADOW}" internal-js
(cd "${SHADOW}" && npm publish --tag gate >/dev/null)
[ "$(versions "${BASE}/npm/proxy-first/is-number")" = "999.0.0-gate" ] ||
  fail "proxy-first packument for is-number is not ONLY 999.0.0-gate (the proxy's versions leaked in)"
echo "  is-number through proxy-first lists only 999.0.0-gate"

# Hosted @types/sindri-gate claims the whole @types scope.
SCOPE="${WORK}/scope"
mkdir -p "${SCOPE}"
echo '{ "name": "@types/sindri-gate", "version": "1.0.0", "license": "MIT" }' > "${SCOPE}/package.json"
write_npmrc "${SCOPE}" internal-js
(cd "${SCOPE}" && npm publish --access public >/dev/null)
# Control: npmjs really has @types/node, and the proxy alone serves it.
[ "$(status "${BASE}/npm/npm/@types%2fnode")" = "200" ] ||
  fail "control failed: the npm proxy alone does not serve @types/node"
code="$(status "${BASE}/npm/proxy-first/@types%2fnode")"
[ "${code}" = "404" ] || fail "@types/node through proxy-first answered ${code}, want 404"
echo "  @types/node: 200 through the proxy alone, 404 through proxy-first"

echo "M9 npm gate passed"
