#!/usr/bin/env bash
# M10 migration gate: docs/migrate/verdaccio.md, end to end, against the
# Compose stack and a real Verdaccio.
#
#   make up && make build && ./scripts/migrate-verdaccio-gate.sh
#
# Needs Docker, node, and network access to registry.npmjs.org (npm itself is
# installed from there at a pinned version, and the page's public member really
# proxies it).
#
# The commands and configuration are read out of the page, between its
# <!-- migrate:NAME --> markers, not copied into this script, so the page cannot
# drift from what passed. The only edit made to them is replacing the page's
# example hosts (verdaccio.example.com, sindri.example.com) with the real ones,
# writing replace-registry-host's value without a port, and, for the lockfile
# command, `rm package-lock.json` as `rm -f` (that copy has no lockfile left).
# The order follows the page: the history is imported, tags included, before any
# client is switched or anything is published to hosted.
#
# Verdaccio runs from a pinned image and requires a login for @acme/*. Sindri
# authenticates to it with a `token:` credential made by `npm token create`
# (SIN-53), so the gate fails if the proxy cannot send a Bearer token.
#
# The server uses :8080 because the page's publicUrl is rewritten to
# http://localhost:8080 and the packuments carry that host.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
PAGE="${ROOT}/docs/migrate/verdaccio.md"
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
HOST="localhost:8080"
BASE="http://${HOST}"
# Verdaccio 6.11.1. Bump the digest on purpose, not by accident.
VERDACCIO_IMAGE="verdaccio/verdaccio@sha256:82f4decaf1c9eff885bf7a1227335f78581ff715288d5f38898e67e28b3bfbdd"
# The npm the gate runs, installed into a temp prefix. replace-registry-host,
# the page's lockfile advice, is behaviour of this npm; check any other.
NPM_VERSION="${MIGRATE_NPM_VERSION:-10.9.9}"
NODE_MAJOR="${MIGRATE_NODE_MAJOR:-22}"
VC="sindri-migrate-gate-verdaccio"

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
  docker rm -fv "${VC}" "${VC}-public" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker rm -fv "${VC}" >/dev/null 2>&1 || true

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
mkdir -p "${HOME}"

echo "== tools: npm ${NPM_VERSION} from npmjs into ${WORK}/tools"
[ "$(node -p 'process.versions.node.split(".")[0]')" = "${NODE_MAJOR}" ] ||
  fail "node $(node -v): the gate is pinned to node ${NODE_MAJOR} (set MIGRATE_NODE_MAJOR to override)"
mkdir -p "${WORK}/tools"
npm install --prefix "${WORK}/tools" --no-package-lock "npm@${NPM_VERSION}" >/dev/null
export PATH="${WORK}/tools/node_modules/.bin:${PATH}"
[ "$(npm -v)" = "${NPM_VERSION}" ] || fail "npm is $(npm -v), want ${NPM_VERSION}"
echo "node $(node -v), npm $(npm -v), verdaccio ${VERDACCIO_IMAGE##*@}"

echo "== 1. start Verdaccio, requiring a login for @acme/*"
cat > "${WORK}/verdaccio.yaml" <<'YML'
storage: /verdaccio/storage/data
auth:
  htpasswd:
    file: /verdaccio/storage/htpasswd
    max_users: 100
packages:
  '@acme/*':
    access: $authenticated
    publish: $authenticated
  '**':
    access: $authenticated
    publish: $authenticated
log: { type: stdout, format: pretty, level: http }
YML
chmod 644 "${WORK}/verdaccio.yaml"
docker create --name "${VC}" -p 127.0.0.1::4873 "${VERDACCIO_IMAGE}" >/dev/null
docker cp "${WORK}/verdaccio.yaml" "${VC}:/verdaccio/conf/config.yaml"
docker start "${VC}" >/dev/null
VPORT="$(docker port "${VC}" 4873/tcp | head -1 | sed 's/.*://')"
VHOST="127.0.0.1:${VPORT}"
VURL="http://${VHOST}"
for _ in $(seq 1 100); do
  curl -fsS "${VURL}/-/ping" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${VURL}/-/ping" >/dev/null || { docker logs "${VC}" >&2; fail "Verdaccio did not start"; }

# adduser USER PASSWORD prints the login token Verdaccio answers with: what
# `npm login` stores. It is how the gate signs in non-interactively.
adduser() {
  curl -fsS -X PUT -H 'Content-Type: application/json' \
    -d "{\"name\":\"$1\",\"password\":\"$2\",\"email\":\"$1@example.com\",\"type\":\"user\"}" \
    "${VURL}/-/user/org.couchdb.user:$1" | node -p 'JSON.parse(require("fs").readFileSync(0)).token'
}

# page_block NAME: the fenced block between the page's markers, with the page's
# example hosts replaced by the real ones.
page_block() {
  local out
  out="$(awk -v n="<!-- migrate:$1 -->" -v e="<!-- /migrate:$1 -->" '$0==n{f=1;next}$0==e{f=0}f' "${PAGE}" |
    sed '1d;$d' |
    sed -e "s#https://verdaccio.example.com#${VURL}#g" -e "s#//verdaccio.example.com/#//${VHOST}/#g" \
      -e "s#^replace-registry-host=verdaccio.example.com\$#replace-registry-host=${VHOST%%:*}#" \
      -e "s#https://sindri.example.com#${BASE}#g" -e "s#//sindri.example.com/#//${HOST}/#g")"
  [ -n "${out}" ] || fail "docs/migrate/verdaccio.md has no migrate:$1 block"
  if grep -q 'example\.com' <<<"${out}"; then
    fail "the migrate:$1 block names a host the gate does not substitute"
  fi
  printf '%s\n' "${out}"
}

PASS_ALICE="alice-gate-password"
PASS_PROXY="proxy-gate-password"
ALICE_LOGIN="$(adduser alice "${PASS_ALICE}")"
PROXY_LOGIN="$(adduser sindri-proxy "${PASS_PROXY}")"

# make_token USER PASSWORD LOGIN: run the page's token command as that user,
# signed in the way `npm login` leaves it, and print the token it creates. The
# password goes to npm on stdin, as the prompt reads it.
make_token() {
  printf '//%s/:_authToken=%s\n' "${VHOST}" "$3" > "${npm_config_userconfig}"
  page_block token > "${WORK}/token.sh"
  printf '%s\n' "$2" | bash "${WORK}/token.sh" 2>/dev/null | awk '/Created .* token/{print $NF}'
  : > "${npm_config_userconfig}"
}
VERDACCIO_TOKEN="$(make_token sindri-proxy "${PASS_PROXY}" "${PROXY_LOGIN}")"
[ -n "${VERDACCIO_TOKEN}" ] || fail "the page's npm token create did not print a token"
export VERDACCIO_TOKEN

# Verdaccio really requires a login: the token works, no token does not.
vcode() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
[ "$(vcode "${VURL}/@acme%2flegacy")" = "401" ] || fail "Verdaccio does not require a login for @acme/*"
[ "$(vcode -H "Authorization: Bearer ${VERDACCIO_TOKEN}" "${VURL}/-/whoami")" = "200" ] ||
  fail "the token npm token create printed is not accepted as a Bearer token"

# as_alice COMMAND...: run a command signed in to Verdaccio as alice.
as_alice() {
  printf '//%s/:_authToken=%s\n' "${VHOST}" "${ALICE_LOGIN}" > "${npm_config_userconfig}"
  "$@"
  local rc=$?
  : > "${npm_config_userconfig}"
  return ${rc}
}

# publish_to_verdaccio NAME VERSION [TAG]: as alice, directly to Verdaccio.
publish_to_verdaccio() {
  local dir="${WORK}/vpub-$1-$2"
  mkdir -p "${dir}"
  printf '{ "name": "@acme/%s", "version": "%s", "main": "index.js", "license": "MIT" }\n' "$1" "$2" > "${dir}/package.json"
  printf "module.exports = 'verdaccio %s %s';\n" "$1" "$2" > "${dir}/index.js"
  printf 'registry=%s/\n' "${VURL}" > "${dir}/.npmrc"
  printf '//%s/:_authToken=%s\n' "${VHOST}" "${ALICE_LOGIN}" > "${npm_config_userconfig}"
  (cd "${dir}" && npm publish --access public ${3:+--tag "$3"} >/dev/null)
  : > "${npm_config_userconfig}"
}
publish_to_verdaccio legacy 1.0.0
publish_to_verdaccio other 1.0.0
echo "  published @acme/legacy@1.0.0 and @acme/other@1.0.0 to Verdaccio"

echo "== 2. the before: install a lockfile against Verdaccio"
BEFORE="${WORK}/before"
mkdir -p "${BEFORE}"
echo '{ "name": "app", "version": "0.0.0", "private": true, "dependencies": { "@acme/legacy": "1.0.0" } }' > "${BEFORE}/package.json"
printf 'registry=%s/\n' "${VURL}" > "${BEFORE}/.npmrc"
printf '//%s/:_authToken=%s\n' "${VHOST}" "${ALICE_LOGIN}" > "${npm_config_userconfig}"
(cd "${BEFORE}" && npm install >/dev/null)
: > "${npm_config_userconfig}"
grep -q "\"resolved\": \"${VURL}/@acme/legacy/-/legacy-1.0.0.tgz\"" "${BEFORE}/package-lock.json" ||
  fail "the before lockfile does not resolve through Verdaccio"
[ -f "${BEFORE}/node_modules/@acme/legacy/index.js" ] || fail "the before install did not land @acme/legacy"
echo "  @acme/legacy@1.0.0 installed from Verdaccio; the lockfile names ${VURL}"

echo "== 3. configure Sindri from the page's own blocks and start it"
{
  # The server settings the page leaves to the deployment.
  cat <<YML
listen: :8080
storage: \${SINDRI_DATA_DIR}
database: \${SINDRI_DATABASE}
auth: \${SINDRI_TOKEN}
YML
  page_block config
} > "${WORK}/sindri.yaml"
export SINDRI_DATABASE
SINDRI_DATABASE="$(fresh_db sindri_migrate_verdaccio)"
SINDRI_DATA_DIR="$(mktemp -d "${WORK}/data.XXXXXX")"
export SINDRI_DATA_DIR
"${BIN}" config check -f "${WORK}/sindri.yaml" >/dev/null || fail "the page's configuration does not pass config check"
"${BIN}" serve --config "${WORK}/sindri.yaml" > "${WORK}/sindri.log" 2>&1 &
PIDS+=("$!")
wait_ready "${BASE}" || { cat "${WORK}/sindri.log" >&2; exit 1; }
if grep -q "${VERDACCIO_TOKEN}" "${WORK}/sindri.log"; then fail "the Verdaccio token is in Sindri's log"; fi
echo "  serving the page's configuration"

AUTH=(-H "Authorization: Bearer ${SINDRI_TOKEN}")
status() { curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$1"; }
versions() {
  curl -fsS "${AUTH[@]}" "$1" |
    python3 -c 'import json,sys; print(" ".join(sorted(json.load(sys.stdin)["versions"])))'
}
# A client's user-level .npmrc, as the page's user-npmrc block writes it. The
# token is npm's ${SINDRI_TOKEN} reference, expanded by npm, not by the shell.
page_block user-npmrc > "${npm_config_userconfig}"
cp "${npm_config_userconfig}" "${HOME}/.npmrc"
SINDRI_USER_NPMRC="$(cat "${npm_config_userconfig}")"
if grep -q "${VHOST}" "${npm_config_userconfig}"; then fail "the client config holds a Verdaccio address or token"; fi

# A client run must not be satisfied by a tarball an earlier one cached (npm's
# cache is keyed by integrity, not by URL), so each starts with an empty cache.
CACHES=0
fresh_cache() { CACHES=$((CACHES + 1)); export npm_config_cache="${WORK}/npm-cache-${CACHES}"; }

# new_client DIR [FILTER]: a project with the before's package.json and
# lockfile, and the page's project .npmrc (minus any line matching FILTER).
new_client() {
  mkdir -p "$1"
  cp "${BEFORE}/package.json" "${BEFORE}/package-lock.json" "$1/"
  page_block project-npmrc | { if [ -n "${2:-}" ]; then grep -v "$2" || true; else cat; fi; } > "$1/.npmrc"
}

echo "== 4. import the history, tags included, before any client is switched"
# Out-of-order versions, one that sorts differently as text and as a number
# (1.10.0 against 1.2.0), and a prerelease. Verdaccio's tags are set to what a
# long-lived package has: latest on the highest release, a named tag on an older
# one, a tag on the prerelease.
publish_to_verdaccio legacy 1.2.0
publish_to_verdaccio legacy 1.1.0
publish_to_verdaccio legacy 1.10.0
publish_to_verdaccio legacy 2.0.0-beta.1 beta
as_alice npm dist-tag add @acme/legacy@1.10.0 latest --registry "${VURL}/" >/dev/null
as_alice npm dist-tag add @acme/legacy@1.1.0 stable --registry "${VURL}/" >/dev/null
# A deprecation lives in the packument, not the tarball.
as_alice npm deprecate "@acme/legacy@1.0.0" "use a newer version" --registry "${VURL}/" >/dev/null
IMPORT="${WORK}/import"
mkdir -p "${IMPORT}"
page_block inventory > "${IMPORT}/inventory.mjs"
page_block inventory-run > "${WORK}/inventory-run.sh"
page_block import > "${WORK}/import.sh"
page_block import-tags > "${WORK}/import-tags.sh"
page_block verify-tags > "${WORK}/verify-tags.sh"
# The page's import-npmrc block: the Verdaccio token line the import needs.
{ printf '%s\n' "${SINDRI_USER_NPMRC}"; page_block import-npmrc; } > "${npm_config_userconfig}"
# run_import: the page's inventory, import, and tag copy, in that order.
# PAGE_SIZE=2 makes the page's inventory ask for two hits at a time, so a scope of
# three packages crosses a page boundary (the second run, in step 8).
run_import() {
  export PAGE_SIZE=2
  (cd "${IMPORT}" && bash -e "${WORK}/inventory-run.sh") || fail "the page's inventory commands failed"
  (cd "${IMPORT}" && bash -e "${WORK}/import.sh" >/dev/null) || fail "the page's import commands failed"
  (cd "${IMPORT}" && bash -e "${WORK}/import-tags.sh" >/dev/null) || fail "the page's tag copy failed"
}
# tags_match: what the page's verify block prints for Sindri's hosted repository
# equals Verdaccio's own dist-tags, for every package in the inventory.
tags_match() {
  (cd "${IMPORT}" && bash -e "${WORK}/verify-tags.sh") | sort > "${WORK}/tags.sindri"
  VURL="${VURL}" TOK="${VERDACCIO_TOKEN}" python3 - "${IMPORT}/inventory.txt" <<'PY' | sort > "${WORK}/tags.verdaccio"
import json, os, sys, urllib.parse, urllib.request
for name in sorted({line.split()[0] for line in open(sys.argv[1]) if line.strip()}):
    req = urllib.request.Request(os.environ["VURL"] + "/" + urllib.parse.quote(name, safe="@"), headers={"Authorization": "Bearer " + os.environ["TOK"]})
    print(name)
    for tag, version in json.load(urllib.request.urlopen(req))["dist-tags"].items():
        print(f"{tag}: {version}")
PY
  diff "${WORK}/tags.verdaccio" "${WORK}/tags.sindri" >&2 || fail "the dist-tags on Sindri differ from Verdaccio's"
}
run_import
cat > "${WORK}/inventory.want" <<'LIST'
@acme/legacy 1.0.0
@acme/legacy 1.1.0
@acme/legacy 1.2.0
@acme/legacy 1.10.0
@acme/legacy 2.0.0-beta.1
@acme/other 1.0.0
LIST
diff "${WORK}/inventory.want" "${IMPORT}/inventory.txt" >&2 || fail "the inventory is not every version in ascending semver order"
[ "$(versions "${BASE}/npm/internal-js/@acme%2flegacy")" = "1.0.0 1.1.0 1.10.0 1.2.0 2.0.0-beta.1" ] ||
  fail "hosted does not list the five imported @acme/legacy versions"
[ "$(versions "${BASE}/npm/internal-js/@acme%2fother")" = "1.0.0" ] || fail "hosted does not list the imported @acme/other@1.0.0"
tags_match
grep -q '^latest: 1.10.0$' "${WORK}/tags.sindri" || fail "latest on Sindri is not Verdaccio's 1.10.0: the import order moved it"
if grep -q 'sindri-import' "${WORK}/tags.sindri"; then fail "the throwaway import tag is still on Sindri"; fi
# What is not carried over, as the page says: the deprecation.
curl -fsS "${AUTH[@]}" "${BASE}/npm/internal-js/@acme%2flegacy" | python3 -c '
import json, sys
doc = json.load(sys.stdin)["versions"]["1.0.0"]
sys.exit(1 if "deprecated" in doc else 0)' || fail "the deprecation was carried over: the page says it is not"
curl -fsS -H "Authorization: Bearer ${VERDACCIO_TOKEN}" "${VURL}/@acme%2flegacy" | grep -q '"deprecated"' ||
  fail "control: Verdaccio does not hold the deprecation"
echo "  five versions imported in semver order; dist-tags equal Verdaccio's; the deprecation was not carried over"
printf '%s\n' "${SINDRI_USER_NPMRC}" > "${npm_config_userconfig}"

echo "== 5. the after: the same lockfile, Verdaccio out of the client's reach"
# The client holds no Verdaccio credential and the lockfile says Verdaccio, so
# what the client does with the lockfile is the whole test.
#
# Pitfall first: with only the registry switched, npm keeps the lockfile's host
# and goes to Verdaccio, where it has no credential.
PITFALL="${WORK}/after-pitfall"
fresh_cache
new_client "${PITFALL}" '^replace-registry-host'
if (cd "${PITFALL}" && npm ci --loglevel http >"${WORK}/pitfall.log" 2>&1); then
  fail "npm ci without replace-registry-host did not go to the lockfile's Verdaccio URL"
fi
grep -q "${VHOST}" "${WORK}/pitfall.log" || { cat "${WORK}/pitfall.log" >&2; fail "the pitfall run never contacted Verdaccio"; }
echo "  without replace-registry-host, npm ci went to ${VHOST} and was refused"

AFTER="${WORK}/after"
new_client "${AFTER}"
fresh_cache
# Verdaccio's log lags its requests, so wait for the tarball refusal above to be written
# before marking the place the next run's lines start after.
refusals() { docker logs "${VC}" 2>&1 | grep -c "legacy-1.0.0.tgz', error: authorization required"; }
for _ in $(seq 1 50); do
  [ "$(refusals)" -ge 1 ] && break
  sleep 0.2
done
MARK="$(docker logs "${VC}" 2>&1 | wc -l | tr -d ' ')"
(cd "${AFTER}" && npm ci --loglevel http >"${WORK}/after.log" 2>&1) || { cat "${WORK}/after.log" >&2; fail "npm ci through Sindri failed"; }
[ -f "${AFTER}/node_modules/@acme/legacy/index.js" ] || fail "@acme/legacy did not land after the switch"
# The tarball came through Sindri: npm asked Sindri for it and never named
# Verdaccio, and Verdaccio's own log has the read from Sindri's account and none
# from a client or an anonymous caller.
grep -q "GET 200 ${BASE}/npm/javascript/@acme/legacy/-/legacy-1.0.0.tgz" "${WORK}/after.log" ||
  { cat "${WORK}/after.log" >&2; fail "npm did not fetch the tarball from Sindri's javascript repository"; }
if grep -q "${VHOST}" "${WORK}/after.log"; then fail "npm contacted Verdaccio directly after the switch"; fi
# The history is on Sindri now, so the lockfile's tarball is the imported copy and
# Verdaccio is not asked for it. Its log lags the request a little: settle first.
sleep 1
docker logs "${VC}" 2>&1 | tail -n +"$((MARK + 1))" > "${WORK}/verdaccio-after.log"
if grep -q "legacy-1.0.0.tgz\|user: alice(" "${WORK}/verdaccio-after.log" || grep -q "error:" "${WORK}/verdaccio-after.log"; then
  { cat "${WORK}/verdaccio-after.log" >&2; fail "Verdaccio was asked for the tarball, or refused something, although it is imported"; }
fi
echo "  installed through Sindri; npm never named Verdaccio, which was not asked for the imported tarball"

echo "== 5b. the lockfile: regenerating it from the page's command"
REGEN="${WORK}/regen"
new_client "${REGEN}" '^replace-registry-host'
rm -f "${REGEN}/package-lock.json"
fresh_cache
page_block lockfile > "${WORK}/lockfile.sh"
# The page's first line deletes the lockfile, which this copy no longer has.
sed -i.bak 's#^rm package-lock.json#rm -f package-lock.json#' "${WORK}/lockfile.sh"
(cd "${REGEN}" && bash -e "${WORK}/lockfile.sh" >/dev/null)
grep -q "\"resolved\": \"${BASE}/npm/javascript/@acme/legacy/-/legacy-1.0.0.tgz\"" "${REGEN}/package-lock.json" ||
  fail "the regenerated lockfile does not resolve through Sindri"
if grep -q "${VHOST}" "${REGEN}/package-lock.json"; then fail "the regenerated lockfile still names Verdaccio"; fi
rm -rf "${REGEN}/node_modules"
fresh_cache
page_block install > "${WORK}/install.sh"
(cd "${REGEN}" && bash -e "${WORK}/install.sh" >/dev/null) || fail "npm ci on the regenerated lockfile failed"
echo "  regenerated lockfile resolves through ${BASE}/npm/javascript/ and installs without replace-registry-host"

echo "== 6. publish @acme/legacy@1.0.1 to hosted, after the import"
PUB="${WORK}/publish"
mkdir -p "${PUB}"
echo '{ "name": "@acme/legacy", "version": "1.0.1", "main": "index.js", "license": "MIT" }' > "${PUB}/package.json"
echo "module.exports = 'sindri legacy 1.0.1';" > "${PUB}/index.js"
page_block publish-npmrc > "${PUB}/.npmrc"
page_block publish > "${WORK}/publish.sh"
(cd "${PUB}" && bash -e "${WORK}/publish.sh" >/dev/null) || fail "the page's publish command failed"
[ "$(versions "${BASE}/npm/javascript/@acme%2flegacy")" = "1.0.0 1.0.1 1.1.0 1.10.0 1.2.0 2.0.0-beta.1" ] ||
  fail "javascript does not list the imported @acme/legacy versions and 1.0.1"
NEW="${WORK}/new"
mkdir -p "${NEW}"
echo '{ "name": "app2", "version": "0.0.0", "private": true, "dependencies": { "@acme/legacy": "1.0.1" } }' > "${NEW}/package.json"
page_block project-npmrc > "${NEW}/.npmrc"
fresh_cache
(cd "${NEW}" && npm install >/dev/null) || fail "installing a version published to hosted failed with the page's project .npmrc"
[ "$(node -p "require('${NEW}/node_modules/@acme/legacy/package.json').version")" = "1.0.1" ] ||
  fail "@acme/legacy@1.0.1 did not install through the virtual repository"
grep -q "sindri legacy 1.0.1" "${NEW}/node_modules/@acme/legacy/index.js" || fail "installed 1.0.1 is not the published file"
fresh_cache
# The page warns off replace-registry-host=always: it rewrites Sindri's own
# tarball links too, and doubles the repository path in them.
ALWAYS="${WORK}/always"
mkdir -p "${ALWAYS}"
cp "${NEW}/package.json" "${ALWAYS}/"
printf 'registry=%s/npm/javascript/\nreplace-registry-host=always\n' "${BASE}" > "${ALWAYS}/.npmrc"
fresh_cache
if (cd "${ALWAYS}" && npm install >"${WORK}/always.log" 2>&1); then
  fail "replace-registry-host=always installed a version resolved from Sindri: the page's warning is stale"
fi
grep -q "/npm/javascript/npm/javascript/" "${WORK}/always.log" || { cat "${WORK}/always.log" >&2; fail "replace-registry-host=always failed, but not by doubling the repository path"; }
echo "  replace-registry-host=always doubles the repository path in Sindri's own tarball links, as the page says"
(cd "${AFTER}" && rm -rf node_modules && npm ci >/dev/null) || fail "the old lockfile no longer installs @acme/legacy@1.0.0"
echo "  1.0.0 (imported) and 1.0.1 (new) both install through javascript"

echo "== 7. a public package still installs: Verdaccio answers 404, the npm member serves it"
PUBLIC="${WORK}/public"
mkdir -p "${PUBLIC}"
echo '{ "name": "app3", "version": "0.0.0", "private": true, "dependencies": { "is-number": "7.0.0" } }' > "${PUBLIC}/package.json"
page_block project-npmrc > "${PUBLIC}/.npmrc"
(cd "${PUBLIC}" && npm install >/dev/null)
[ "$(node -p "require('${PUBLIC}/node_modules/is-number/package.json').version")" = "7.0.0" ] ||
  fail "is-number@7.0.0 did not install through javascript"
echo "  is-number@7.0.0 installed from the npm member"

echo "== 8. the name-claim rule: a hosted name is never served from the Verdaccio proxy"
# The control repository is a second proxy over the same Verdaccio with its own
# cache, so it shows what Verdaccio holds now, not what verdaccio cached earlier.
api() { curl -s -o /dev/null -w '%{http_code}' -X POST "${AUTH[@]}" -H 'Content-Type: application/json' -d "$2" "${BASE}/api/v1/repositories" | grep -qx "$1"; }
api 201 "{\"name\":\"verdaccio-fresh\",\"type\":\"proxy\",\"protocol\":\"npm\",\"upstream_url\":\"${VURL}\",\"credentials_ref\":\"verdaccio\",\"read\":[\"authenticated\"]}" ||
  fail "creating the verdaccio-fresh control proxy failed"
# proxy-first lists the proxy BEFORE the hosted member, so only the claim keeps
# the hosted package in front.
api 201 '{"name":"proxy-first","type":"virtual","protocol":"npm","members":["verdaccio","internal-js"],"read":["authenticated"]}' ||
  fail "creating proxy-first failed"
publish_to_verdaccio legacy 1.0.2
[ "$(versions "${BASE}/npm/verdaccio-fresh/@acme%2flegacy")" = "1.0.0 1.0.2 1.1.0 1.10.0 1.2.0 2.0.0-beta.1" ] ||
  fail "control: the proxy alone does not list Verdaccio's versions, 1.0.2 included"
[ "$(versions "${BASE}/npm/proxy-first/@acme%2flegacy")" = "1.0.0 1.0.1 1.1.0 1.10.0 1.2.0 2.0.0-beta.1" ] ||
  fail "@acme/legacy through proxy-first is not exactly the hosted versions (Verdaccio's 1.0.2 leaked in)"
MARK="$(docker logs "${VC}" 2>&1 | wc -l | tr -d ' ')"
[ "$(status "${BASE}/npm/verdaccio-fresh/@acme/legacy/-/legacy-1.0.2.tgz")" = "200" ] ||
  fail "control: the proxy alone does not serve legacy-1.0.2.tgz"
# The proxy read the tarball with the token credential, as the sindri-proxy account.
for _ in $(seq 1 50); do
  docker logs "${VC}" 2>&1 | tail -n +"$((MARK + 1))" > "${WORK}/verdaccio-proxy.log"
  grep -q "user: sindri-proxy(.*legacy-1.0.2.tgz" "${WORK}/verdaccio-proxy.log" && break
  sleep 0.2
done
grep -q "user: sindri-proxy(.*legacy-1.0.2.tgz" "${WORK}/verdaccio-proxy.log" ||
  { cat "${WORK}/verdaccio-proxy.log" >&2; fail "Verdaccio did not log the tarball read by the sindri-proxy account"; }
code="$(status "${BASE}/npm/proxy-first/@acme/legacy/-/legacy-1.0.2.tgz")"
[ "${code}" = "404" ] || fail "legacy-1.0.2.tgz through proxy-first answered ${code}, want 404"
echo "  @acme/legacy: the proxy alone has 1.0.2; proxy-first lists only the hosted versions"

# The whole scope is claimed: @acme/late was published to Verdaccio after the import.
publish_to_verdaccio late 1.0.0
[ "$(status "${BASE}/npm/verdaccio-fresh/@acme%2flate")" = "200" ] ||
  fail "control: the proxy alone does not serve @acme/late"
code="$(status "${BASE}/npm/javascript/@acme%2flate")"
[ "${code}" = "404" ] || fail "@acme/late through javascript answered ${code}, want 404: the scope is not claimed"
echo "  @acme/late: 200 through the proxy alone, 404 through javascript, until it is imported"
# The page: run the inventory, import and tag copy again. Only what is new goes in.
{ printf '%s\n' "${SINDRI_USER_NPMRC}"; page_block import-npmrc; } > "${npm_config_userconfig}"
run_import
printf '%s\n' "${SINDRI_USER_NPMRC}" > "${npm_config_userconfig}"
cat > "${WORK}/inventory.want2" <<'LIST'
@acme/late 1.0.0
@acme/legacy 1.0.0
@acme/legacy 1.0.2
@acme/legacy 1.1.0
@acme/legacy 1.2.0
@acme/legacy 1.10.0
@acme/legacy 2.0.0-beta.1
@acme/other 1.0.0
LIST
diff "${WORK}/inventory.want2" "${IMPORT}/inventory.txt" >&2 || fail "the second inventory is not what Verdaccio holds now"
# The control: one search page holds fewer than the three packages, so the listing
# above could only be complete by going on to the next page.
[ "$(curl -fsS -H "Authorization: Bearer ${VERDACCIO_TOKEN}" "${VURL}/-/v1/search?text=%40acme&size=2&from=0" |
  python3 -c 'import json,sys; print(len(json.load(sys.stdin)["objects"]))')" -lt 3 ] ||
  fail "control: a search page of two held all three packages, so no page boundary was crossed"
tags_match
if grep -q 'sindri-import' "${WORK}/tags.sindri"; then fail "the throwaway import tag is still on Sindri after the second run"; fi
echo "  @acme/late: 200 through javascript once imported; the second run imported only what was new"

echo "== 9. a Verdaccio that advertises another public URL: the tarball host rule"
# Verdaccio writes its public URL into tarball links. Sindri fetches a tarball
# only from the upstream's own host (the token goes with the request), so the
# upstream URL must be the public one, as the page says. Anonymous access here,
# so the credential plays no part.
sed 's/access: \$authenticated/access: $all/' "${WORK}/verdaccio.yaml" > "${WORK}/verdaccio-public.yaml"
chmod 644 "${WORK}/verdaccio-public.yaml"
docker create --name "${VC}-public" -e VERDACCIO_PUBLIC_URL=http://npm.corp.test -p 127.0.0.1::4873 "${VERDACCIO_IMAGE}" >/dev/null
docker cp "${WORK}/verdaccio-public.yaml" "${VC}-public:/verdaccio/conf/config.yaml"
docker start "${VC}-public" >/dev/null
PURL="http://127.0.0.1:$(docker port "${VC}-public" 4873/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 100); do curl -fsS "${PURL}/-/ping" >/dev/null 2>&1 && break; sleep 0.2; done
PLOGIN="$(curl -fsS -X PUT -H 'Content-Type: application/json' -d '{"name":"carol","password":"carol-gate-password","email":"c@example.com","type":"user"}' \
  "${PURL}/-/user/org.couchdb.user:carol" | node -p 'JSON.parse(require("fs").readFileSync(0)).token')"
PPUB="${WORK}/ppub"
mkdir -p "${PPUB}"
echo '{ "name": "@acme/z", "version": "1.0.0", "license": "MIT" }' > "${PPUB}/package.json"
printf 'registry=%s/\n//%s/:_authToken=%s\n' "${PURL}" "${PURL#http://}" "${PLOGIN}" > "${PPUB}/.npmrc"
(cd "${PPUB}" && npm_config_userconfig=/dev/null npm publish --access public >/dev/null)
api 201 "{\"name\":\"verdaccio-public\",\"type\":\"proxy\",\"protocol\":\"npm\",\"upstream_url\":\"${PURL}\",\"read\":[\"authenticated\"]}" ||
  fail "creating the verdaccio-public proxy failed"
[ "$(status "${BASE}/npm/verdaccio-public/@acme%2fz")" = "200" ] || fail "the packument from the second Verdaccio is not served"
code="$(status "${BASE}/npm/verdaccio-public/@acme/z/-/z-1.0.0.tgz")"
[ "${code}" = "502" ] || fail "a tarball link on another host answered ${code}, want 502"
grep -q 'tarball URL is on another host' "${WORK}/sindri.log" || fail "Sindri's log does not say the tarball host was refused"
echo "  a tarball link on npm.corp.test is refused (502) when the upstream is ${PURL#http://}, as the page says"

if grep -q "${VERDACCIO_TOKEN}" "${WORK}/sindri.log"; then fail "the Verdaccio token is in Sindri's log"; fi
echo "M10 Verdaccio migration gate passed"
