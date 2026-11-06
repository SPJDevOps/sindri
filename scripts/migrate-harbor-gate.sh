#!/usr/bin/env bash
# M10 migration gate: docs/migrate/harbor.md, end to end, against the Compose
# stack and a registry standing in for Harbor.
#
#   make up && make build && ./scripts/migrate-harbor-gate.sh
#
# Needs Docker, curl, openssl, tar, and python3, and no network beyond pulling the
# pinned images. Docker Desktop must list
# host.docker.internal:8080 as an insecure registry, as scripts/oci-docker-gate.sh
# and scripts/m8-gc-gate.sh already require. Every image is pinned by digest.
#
# THIS IS NOT A TEST AGAINST HARBOR. The stand-in is the Distribution registry
# (registry:2, 2.8.3, the engine Harbor is built on) behind nginx, with
# cesanta/docker_auth as its token server, and nginx puts the registry's /v2/ and
# the token endpoint on one origin, as Harbor does. That reproduces Harbor's
# robot-account flow: an anonymous request is refused with a Bearer challenge, a
# robot name and secret are exchanged at the token realm for a short-lived token,
# and the token is presented to /v2/. It has none of Harbor's own API, projects,
# quotas, replication, scanner, or redirect-to-S3 blob downloads, and a real
# Harbor can behave differently in ways this does not show.
#
# The commands and configuration are read out of the page, between its
# <!-- migrate:NAME --> markers, not copied into this script, so the page cannot
# drift from what passed. The edits made to them are only these:
#   - harbor.example.com becomes the stand-in (http://127.0.0.1:PORT where the
#     command runs on this host, host.docker.internal:PORT where a container or
#     the Docker daemon reaches it) and sindri.example.com becomes the server on
#     :8080 (http://localhost:8080 for curl, host.docker.internal:8080 for docker
#     and crane). The page says https; the gate runs plain http.
#   - the page's commands run in the page's order: the history is inventoried and
#     copied before any client is switched or anything is pushed to hosted.
#   - crane runs from a pinned image through a wrapper that adds --insecure,
#     because both registries here speak http. No other crane flag is added.
#
# What this isolates, and what it does not. The stand-in and crane run in
# containers on a private Docker network or with an empty Docker config, and
# nothing of the machine's registry credentials is read by crane. Sindri is a
# process on this host, and the Docker daemon that pulls from it is the machine's
# own Docker Desktop daemon, so its image cache and its insecure-registry list are
# the machine's: the gate removes the images it pulls before it pulls them again.
# The host's network is not isolated.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
BIN=./bin/sindri
PAGE="${ROOT}/docs/migrate/harbor.md"
export SINDRI_TOKEN="${SINDRI_TOKEN:-sindri-dev-token}"
PG_CONTAINER="${PG_CONTAINER:-sindri-dev-postgres-1}"
PG_PORT="${SINDRI_PG_PORT:-55432}"
HOST="localhost:8080"
BASE="http://${HOST}"
# What a container, and Docker Desktop's daemon, call the host.
CHOST="host.docker.internal"
CSINDRI="${CHOST}:8080"
# registry 2.8.3, docker_auth 1.14.0, nginx 1.28, crane v0.21.0, alpine 3.20 and
# Bump a digest on purpose, not by accident.
REGISTRY_IMAGE="registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
AUTH_IMAGE="cesanta/docker_auth@sha256:98e0307e0d2dcbafe10098814a97d248143b78ca478693f78c3c0d67fe776ab4"
NGINX_IMAGE="nginx@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236"
CRANE_IMAGE="gcr.io/go-containerregistry/crane@sha256:ce0d9cffa63d62c547b7df1cad9875fe90e8f7e2413870f0b50ca03839f56901"
NET="sindri-migrate-gate-harbor-net"
RC="sindri-migrate-gate-harbor-registry"
AC="sindri-migrate-gate-harbor-auth"
NC="sindri-migrate-gate-harbor-nginx"
OWN=("${RC}" "${AC}" "${NC}")

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
  # The stand-in's own account of what it saw, in the gate's log_format.
  docker logs "${NC}" 2>&1 | grep -E '^(anonymous|robot|seed|bearer|other) ' | tail -8 >&2 || true
  [ -z "${GATE_HOLD:-}" ] || { echo "holding for ${GATE_HOLD}s" >&2; sleep "${GATE_HOLD}"; }
  exit 1
}

# A listener on 8080 is not ours to kill, and a second server could not bind.
if curl -s -o /dev/null --max-time 2 "${BASE}/readyz"; then
  fail "something is already listening on ${HOST}; stop it and rerun"
fi

WORK="$(mktemp -d)"
PIDS=()
LOGGED_IN=0
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "${p}" 2>/dev/null || true; done
  [ "${LOGGED_IN}" = 0 ] || docker logout "${CSINDRI}" >/dev/null 2>&1 || true
  docker rm -f "${OWN[@]}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker rm -f "${OWN[@]}" >/dev/null 2>&1 || true
docker network rm "${NET}" >/dev/null 2>&1 || true

# Gate-only secrets for a throwaway stand-in. The bcrypt hashes are of these.
ROBOT_USER='robot$acme+sindri'
export HARBOR_ROBOT_PASSWORD="robot-gate-password"
ROBOT_HASH='$2y$10$s9i0A9zYmLpDOSb77FoNkO.B0wzOz4dLTAhgm6woevcyVejjhMMm2'
SEED_PASSWORD="seed-gate-password"
SEED_HASH='$2y$10$QMfE.yo12S5.t3K/kufQ1O5AC3PUSFTbTXdUtifqacA/Jmn1O7jtG'
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }

echo "== 1. the stand-in: registry 2.8.3 and docker_auth behind one nginx origin"
docker network create "${NET}" >/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=harbor-gate-token-issuer" \
  -keyout "${WORK}/server.key" -out "${WORK}/server.pem" >/dev/null 2>&1
chmod 644 "${WORK}/server.key" "${WORK}/server.pem"
# The robot is named the way Harbor names one (robot$project+name) and may pull
# from the acme project only. Nobody is anonymous: there is no "" user.
cat > "${WORK}/auth_config.yml" <<YML
server:
  addr: ":5001"
token:
  issuer: "harbor-gate-issuer"
  expiration: 900
  certificate: "/config/server.pem"
  key: "/config/server.key"
users:
  "${ROBOT_USER}":
    password: '${ROBOT_HASH}'
  "seed":
    password: '${SEED_HASH}'
acl:
  - match: {account: "seed"}
    actions: ["*"]
  - match: {account: "${ROBOT_USER}", type: repository, name: "acme/*"}
    actions: ["pull"]
  - match: {account: "${ROBOT_USER}", type: registry, name: "catalog"}
    actions: ["*"]
YML
chmod 644 "${WORK}/auth_config.yml"
docker create --name "${AC}" --network "${NET}" --network-alias auth "${AUTH_IMAGE}" >/dev/null
# The image has no /config; copying a directory to a path that does not exist
# creates it.
mkdir "${WORK}/authcfg"
cp "${WORK}/auth_config.yml" "${WORK}/server.pem" "${WORK}/server.key" "${WORK}/authcfg/"
docker cp "${WORK}/authcfg" "${AC}:/config"
docker start "${AC}" >/dev/null
# The registry's realm is a bare path, and nginx completes it with the Host the
# caller used (see nginx.conf below), so Sindri, crane and the Docker daemon each
# reach the token endpoint under the name they used for the registry. (Harbor
# does this from its external URL; the stand-in cannot know its published port
# before it starts.)
docker create --name "${RC}" --network "${NET}" --network-alias registry \
  -e REGISTRY_AUTH=token -e REGISTRY_AUTH_TOKEN_REALM=/service/token \
  -e REGISTRY_AUTH_TOKEN_SERVICE=harbor-registry -e REGISTRY_AUTH_TOKEN_ISSUER=harbor-gate-issuer \
  -e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/etc/docker/registry/server.pem \
  -e REGISTRY_STORAGE_DELETE_ENABLED=false "${REGISTRY_IMAGE}" >/dev/null
docker cp "${WORK}/server.pem" "${RC}:/etc/docker/registry/server.pem"
docker start "${RC}" >/dev/null
# nginx's log names who made each request by matching the Authorization header
# against the gate's two known logins, so a log line never holds a credential.
cat > "${WORK}/nginx.conf" <<CONF
map_hash_bucket_size 256;
map \$http_authorization \$who {
    default                                           other;
    ""                                                anonymous;
    "~^Bearer "                                       bearer;
    "Basic $(b64 "${ROBOT_USER}:${HARBOR_ROBOT_PASSWORD}")" robot;
    "Basic $(b64 "seed:${SEED_PASSWORD}")"            seed;
}
map \$upstream_http_www_authenticate \$challenge {
    "~^Bearer realm=\"/service/token\"(?<rest>.*)\$"  'Bearer realm="http://\$http_host/service/token"\$rest';
    default                                          "";
}
log_format who '\$who \$request_method \$uri \$status';
access_log /dev/stdout who;
server {
    listen 8080;
    client_max_body_size 0;
    proxy_http_version 1.1;
    proxy_set_header Host \$http_host;
    proxy_request_buffering off;
    proxy_buffering off;
    location = /service/token { proxy_pass http://auth:5001/auth; }
    location /v2/ {
        proxy_pass http://registry:5000;
        proxy_hide_header WWW-Authenticate;
        add_header WWW-Authenticate \$challenge always;
    }
}
CONF
docker create --name "${NC}" --network "${NET}" -p 127.0.0.1::8080 "${NGINX_IMAGE}" >/dev/null
docker cp "${WORK}/nginx.conf" "${NC}:/etc/nginx/conf.d/default.conf"
docker start "${NC}" >/dev/null
sleep 1
[ "$(docker inspect -f '{{.State.Running}}' "${NC}")" = "true" ] || { docker logs "${NC}" >&2; fail "nginx did not start"; }
HPORT="$(docker port "${NC}" 8080/tcp | head -1 | sed 's/.*://')"
HLOCAL="127.0.0.1:${HPORT}"
HFROMC="${CHOST}:${HPORT}"
for _ in $(seq 1 100); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://${HLOCAL}/v2/")" = "401" ] && break
  sleep 0.2
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "http://${HLOCAL}/v2/")" = "401" ] ||
  { docker logs "${RC}" >&2; docker logs "${NC}" >&2; fail "the stand-in did not start"; }
challenge="$(curl -s -D - -o /dev/null "http://${HLOCAL}/v2/" | tr -d '\r' | grep -i '^www-authenticate:')"
# A bearer challenge whose realm is on the same origin the request used: that is
# what lets Sindri send the robot's login to it over http (see the page).
grep -q "realm=\"http://${HLOCAL}/service/token\"" <<<"${challenge}" ||
  fail "the registry's challenge is not a Bearer realm on the stand-in's origin: ${challenge}"
[ "$(curl -s -o /dev/null -w '%{http_code}' "http://${HLOCAL}/service/token?service=harbor-registry")" = "401" ] ||
  fail "the token endpoint serves an anonymous caller"
echo "  anonymous /v2/ is refused with a Bearer challenge; the token endpoint refuses anonymous callers"

# page_block NAME [server|client]: the fenced block between the page's markers,
# with the page's example hosts replaced by the real ones. "server" names the
# hosts as a command on this host sees them, "client" as a container or the
# Docker daemon does.
page_block() {
  local out hurl surl hbare sbare
  if [ "${2:-client}" = server ]; then
    hurl="http://${HLOCAL}"
    surl="${BASE}"
    hbare="${HLOCAL}"
    sbare="${HOST}"
  else
    hurl="http://${HFROMC}"
    surl="http://${CSINDRI}"
    hbare="${HFROMC}"
    sbare="${CSINDRI}"
  fi
  out="$(awk -v n="<!-- migrate:$1 -->" -v e="<!-- /migrate:$1 -->" '$0==n{f=1;next}$0==e{f=0}f' "${PAGE}" |
    sed '1d;$d' |
    sed -e "s#https://harbor.example.com#${hurl}#g" -e "s#https://sindri.example.com#${surl}#g" \
      -e "s#harbor.example.com#${hbare}#g" -e "s#sindri.example.com#${sbare}#g")"
  [ -n "${out}" ] || fail "docs/migrate/harbor.md has no migrate:$1 block"
  if grep -q 'example\.com' <<<"${out}"; then
    fail "the migrate:$1 block names a host the gate does not substitute"
  fi
  printf '%s\n' "${out}"
}

# crane runs from a pinned image. The wrapper makes it a command the page's
# blocks can call by name; its only addition to the page's arguments is
# --insecure. Its Docker config is a directory of its own, mounted into the
# container, so what `crane auth login` stores never touches the machine's.
mkcrane() { # DIR NAME
  mkdir -p "$1" "${WORK}/bin/$2"
  chmod 777 "$1"
  cat > "${WORK}/bin/$2/crane" <<WRAP
#!/usr/bin/env bash
# stdin is passed only to \`auth login\`, so a loop's input is never read by a container.
flags=(); [ "\${1:-}" = auth ] && flags=(-i)
exec docker run --rm \${flags[@]+"\${flags[@]}"} --add-host ${CHOST}:host-gateway -e DOCKER_CONFIG=/cfg \\
  -v "$1:/cfg" -v "${WORK}/seed:/seed:ro" ${CRANE_IMAGE} --insecure "\$@"
WRAP
  chmod +x "${WORK}/bin/$2/crane"
}
mkcrane "${WORK}/cfg-migrate" migrate
mkcrane "${WORK}/cfg-seed" seed
mkcrane "${WORK}/cfg-before" before
mkcrane "${WORK}/cfg-anon" anon
sc() { PATH="${WORK}/bin/seed:${PATH}" crane "$@"; }
bc() { PATH="${WORK}/bin/before:${PATH}" crane "$@"; }
ac() { PATH="${WORK}/bin/anon:${PATH}" crane "$@"; }

echo "== 2. seed the stand-in: acme/app (two tags), acme/tools, and other/secret the robot may not read"
printf '%s' "${SEED_PASSWORD}" | sc auth login "${HFROMC}" -u seed --password-stdin >/dev/null 2>&1 || fail "seed login failed"
# Each image is a multi-architecture index of two one-layer images, built here
# with crane, so no registry outside the gate is needed and the copy has a real
# index to keep intact. A layer's text makes its digest.
mkdir "${WORK}/seed"
for img in app-1.0 app-1.1 secret-1.0; do
  mkdir "${WORK}/seed-$img" && echo "image ${img}" > "${WORK}/seed-$img/note.txt"
  # No extended attributes: macOS tar would add one the Docker daemon cannot apply.
  COPYFILE_DISABLE=1 tar --no-xattrs --format ustar -cf "${WORK}/seed/${img}.tar" -C "${WORK}/seed-$img" note.txt
done
seed_index() { # LAYER-TAR REPOSITORY:TAG
  local arch
  for arch in amd64 arm64; do
    sc append --oci-empty-base -f "/seed/$1.tar" -t "${HFROMC}/build/$1:${arch}" >/dev/null 2>"${WORK}/seed.err" &&
      sc mutate --set-platform "linux/${arch}" "${HFROMC}/build/$1:${arch}" -t "${HFROMC}/build/$1:${arch}" >/dev/null 2>>"${WORK}/seed.err" ||
      { tail -5 "${WORK}/seed.err" >&2; fail "building the seed image $1 for ${arch} failed"; }
  done
  sc index append -m "${HFROMC}/build/$1:amd64" -m "${HFROMC}/build/$1:arm64" -t "${HFROMC}/$2" >/dev/null 2>"${WORK}/seed.err" ||
    { tail -5 "${WORK}/seed.err" >&2; fail "building the seed index $2 failed"; }
}
seed_index app-1.0 acme/app:1.0
seed_index app-1.1 acme/app:1.1
seed_index secret-1.0 other/secret:1.0
sc copy "${HFROMC}/acme/app:1.0" "${HFROMC}/acme/tools:1.0" >/dev/null 2>&1 || fail "seeding acme/tools:1.0 failed"
D_APP10="$(sc digest "${HFROMC}/acme/app:1.0")"
D_APP11="$(sc digest "${HFROMC}/acme/app:1.1")"
D_TOOLS10="$(sc digest "${HFROMC}/acme/tools:1.0")"
[ "${D_TOOLS10}" = "${D_APP10}" ] || fail "acme/tools:1.0 is not a copy of acme/app:1.0"
echo "  acme/app:1.0 ${D_APP10:0:19}, 1.1 ${D_APP11:0:19}; acme/tools:1.0 ${D_TOOLS10:0:19}"

echo "== 3. the page's robot variables and the before: read the stand-in as the robot"
page_block robot-env > "${WORK}/robot-env.sh"
# shellcheck disable=SC1090
source "${WORK}/robot-env.sh"
[ "${HARBOR_ROBOT_USER}" = "${ROBOT_USER}" ] || fail "the page's quoting did not give the robot name ${ROBOT_USER}: got ${HARBOR_ROBOT_USER}"
printf '%s' "${HARBOR_ROBOT_PASSWORD}" | bc auth login "${HFROMC}" -u "${HARBOR_ROBOT_USER}" --password-stdin >/dev/null 2>&1 ||
  fail "the robot cannot sign in to the stand-in"
[ "$(bc digest "${HFROMC}/acme/app:1.0")" = "${D_APP10}" ] || fail "the robot cannot read acme/app:1.0"
if bc digest "${HFROMC}/other/secret:1.0" >/dev/null 2>&1; then fail "the robot read a project it has no grant for"; fi
if ac digest "${HFROMC}/acme/app:1.0" >/dev/null 2>&1; then fail "the stand-in answered a pull with no credential"; fi
echo "  robot reads acme/app, is refused other/secret, and with no login is refused everything"

echo "== 4. configure Sindri from the page's own blocks and start it"
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
SINDRI_DATABASE="$(fresh_db sindri_migrate_harbor)"
SINDRI_DATA_DIR="$(mktemp -d "${WORK}/data.XXXXXX")"
export SINDRI_DATA_DIR
"${BIN}" config check -f "${WORK}/sindri.yaml" >/dev/null || fail "the page's configuration does not pass config check"
"${BIN}" serve --config "${WORK}/sindri.yaml" > "${WORK}/sindri.log" 2>&1 &
PIDS+=("$!")
wait_ready "${BASE}" || { cat "${WORK}/sindri.log" >&2; exit 1; }
echo "  serving the page's configuration"

echo "== 5. machine tokens from the page: a puller, and a pusher for internal"
export SINDRI_ADMIN_TOKEN="${SINDRI_TOKEN}"
page_block tokens server > "${WORK}/tokens.sh"
bash -e "${WORK}/tokens.sh" > "${WORK}/tokens.out" 2>"${WORK}/tokens.err" || { cat "${WORK}/tokens.err" >&2; fail "the page's token commands failed"; }
# The page prints each response, so the output is a run of JSON objects.
token_named() {
  python3 -c '
import json, sys
text, dec, i = sys.stdin.read(), json.JSONDecoder(), 0
while i < len(text):
    if text[i].isspace():
        i += 1
        continue
    obj, i = dec.raw_decode(text, i)
    if obj["name"] == sys.argv[1]:
        print(obj["token"])
' "$1" < "${WORK}/tokens.out"
}
PULL_TOKEN="$(token_named ci-pull)"
PUSH_TOKEN="$(token_named ci-push)"
[ -n "${PULL_TOKEN}" ] && [ -n "${PUSH_TOKEN}" ] || fail "the page's token commands printed no token"
rm -f "${WORK}/tokens.out"
export SINDRI_PULL_TOKEN="${PULL_TOKEN}" SINDRI_PUSH_TOKEN="${PUSH_TOKEN}"

# nlog: the stand-in's access log, which lags a request a little.
nlog() { docker logs "${NC}" 2>&1; }
settle() { # a grep -E pattern the log must come to contain after MARK
  for _ in $(seq 1 50); do
    nlog | tail -n +"$((MARK + 1))" > "${WORK}/nginx-after.log"
    grep -qE "$1" "${WORK}/nginx-after.log" && return 0
    sleep 0.2
  done
  cat "${WORK}/nginx-after.log" >&2
  return 1
}
# bearer_curl TOKEN CURL-ARGS...: the token goes to curl on stdin (-H @-), as the
# page does, so it is never on a command line. printf is a builtin.
bearer_curl() {
  local tok="$1"
  shift
  printf 'Authorization: Bearer %s\n' "${tok}" | curl -H @- "$@"
}
# status TOKEN CURL-ARGS...: the HTTP status of a request made with a bearer token.
status() {
  local tok="$1"
  shift
  bearer_curl "${tok}" -s -o /dev/null -w '%{http_code}' "$@"
}
ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
manifest_status() { status "${PULL_TOKEN}" -H "Accept: ${ACCEPT}" "${BASE}/v2/$1/manifests/$2"; }

echo "== 6. the inventory, from the page, before any client is switched"
page_block login > "${WORK}/login.sh"
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/login.sh" < /dev/null >/dev/null 2>&1 || fail "the page's crane logins failed"
page_block inventory > "${WORK}/inventory.sh"
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/inventory.sh" < /dev/null > "${WORK}/inventory.out" 2>"${WORK}/inventory.err" ||
  { cat "${WORK}/inventory.err" >&2; fail "the page's inventory commands failed"; }
# The page prints "repository tag" for every tag of every acme repository. The
# gate seeded other/secret too, and the page's grep leaves it out.
sort "${WORK}/inventory.out" > "${WORK}/inventory.sorted"
printf '%s\n' "acme/app 1.0" "acme/app 1.1" "acme/tools 1.0" > "${WORK}/inventory.want"
diff "${WORK}/inventory.want" "${WORK}/inventory.sorted" >&2 || fail "the inventory is not exactly what the acme project holds"
echo "  3 tags in 2 repositories, and not other/secret"

echo "== 7. copy the history into internal, from the page"
page_block copy > "${WORK}/copy.sh"
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/copy.sh" < /dev/null >"${WORK}/copy.out" 2>&1 || { tail -15 "${WORK}/copy.out" >&2; fail "the page's copy failed"; }
# Running it again must not fail: the page uses --no-clobber, so it is repeatable.
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/copy.sh" < /dev/null >/dev/null 2>&1 || fail "the page's copy fails when run again"
page_block verify > "${WORK}/verify.sh"
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/verify.sh" < /dev/null > "${WORK}/verify.out" 2>&1 || { cat "${WORK}/verify.out" >&2; fail "the page's digest comparison failed"; }
[ "$(grep -c '^ok ' "${WORK}/verify.out")" = "3" ] || { cat "${WORK}/verify.out" >&2; fail "the page's digest comparison did not report three matches"; }
# The hosted copy keeps the digest of the index and of the tag that was hosted already.
for ref in "acme/app:1.0 ${D_APP10}" "acme/app:1.1 ${D_APP11}" "acme/tools:1.0 ${D_TOOLS10}"; do
  set -- ${ref}
  got="$(PATH="${WORK}/bin/migrate:${PATH}" crane digest "${CSINDRI}/internal/$1")" || fail "internal/$1 is not in Sindri"
  [ "${got}" = "$2" ] || fail "internal/$1 has digest ${got}, want Harbor's $2"
done
[ "$(manifest_status images/acme/app 1.0)" = "200" ] || fail "images/acme/app:1.0 is still hidden after the copy"
[ "$(manifest_status images/acme/app 1.1)" = "200" ] || fail "images/acme/app:1.1 is still hidden after the copy"
echo "  every tag has Harbor's digest in internal, a second run changes nothing, and images serves all three tags"

echo "== 8. the after: a client pulls through Sindri with no Harbor credential"
MARK="$(nlog | wc -l | tr -d ' ')"
LOGGED_IN=1
export SINDRI_PULL_TOKEN="${PULL_TOKEN}" SINDRI_PUSH_TOKEN="${PUSH_TOKEN}"
page_block pull > "${WORK}/pull.sh"
docker rmi "${CSINDRI}/images/acme/app:1.0" >/dev/null 2>&1 || true
bash -e "${WORK}/pull.sh" > "${WORK}/pull.out" 2>&1 || { cat "${WORK}/pull.out" >&2; fail "the page's docker pull through Sindri failed"; }
grep -q "Digest: ${D_APP10}" "${WORK}/pull.out" || { cat "${WORK}/pull.out" >&2; fail "the image pulled through Sindri is not the digest Harbor holds"; }
# The copy is Sindri's own, so the stand-in is not asked for it: its log, which
# lags a request a little, shows no read of acme/app. (This greps the log_format
# defined above, which the gate owns: "who METHOD uri status".)
sleep 1
nlog | tail -n +"$((MARK + 1))" > "${WORK}/nginx-after.log"
if grep -q 'acme/app' "${WORK}/nginx-after.log"; then
  cat "${WORK}/nginx-after.log" >&2
  fail "the stand-in was asked for acme/app although it is copied and claimed"
fi
# An image only Harbor has, pushed to it after the copy, is read through the proxy
# with the robot's login: the stand-in saw the Basic login at the token realm and a
# Bearer read of the image, and nothing from the client, which has no credential.
sc copy "${HFROMC}/acme/app:1.0" "${HFROMC}/acme/late:1.0" >/dev/null 2>&1 || fail "seeding acme/late:1.0 failed"
MARK="$(nlog | wc -l | tr -d ' ')"
docker rmi "${CSINDRI}/images/acme/late:1.0" >/dev/null 2>&1 || true
docker pull "${CSINDRI}/images/acme/late:1.0" > "${WORK}/pull-late.out" 2>&1 || { cat "${WORK}/pull-late.out" >&2; fail "docker pull of acme/late through Sindri failed"; }
grep -q "Digest: ${D_APP10}" "${WORK}/pull-late.out" || { cat "${WORK}/pull-late.out" >&2; fail "acme/late:1.0 pulled through the proxy is not Harbor's digest"; }
settle "^robot GET /service/token 200$" || fail "the stand-in never logged the robot's token exchange"
settle "^bearer GET /v2/acme/late/manifests/.* 200$" || fail "the stand-in never logged Sindri's read of acme/late"
if grep -E '^(seed|other) ' "${WORK}/nginx-after.log" | grep -q .; then fail "an unexpected login reached the stand-in during the pull"; fi
echo "  acme/app:1.0 pulled from the copy at ${D_APP10:0:19}, the stand-in unasked; acme/late:1.0 pulled through the proxy as the robot"

echo "== 9. a token scoped to one repository cannot read through a virtual repository"
SCOPED="$(bearer_curl "${SINDRI_TOKEN}" -fsS -X POST -H 'Content-Type: application/json' -d '{"name":"gate-scoped","role":"read","repository":"images"}' "${BASE}/api/v1/tokens" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
code="$(status "${SCOPED}" -H "Accept: ${ACCEPT}" "${BASE}/v2/images/acme/app/manifests/1.0")"
[ "${code}" = "404" ] || fail "a token scoped to images read acme/app/1.0 through members it is not scoped to: HTTP ${code}"
echo "  scoped to images: HTTP ${code}"

echo "== 10. the name claim: a push to hosted hides Harbor's tags of an image not yet copied"
docker build -q -t acme-app:2.0 "${ROOT}/testdata/demo" >/dev/null
page_block push > "${WORK}/push.sh"
bash -e "${WORK}/push.sh" >/dev/null 2>&1 || fail "the page's docker push to internal failed"
# harbor is the control: the proxy alone still has the tags Harbor holds.
[ "$(manifest_status harbor/acme/late 1.0)" = "200" ] || fail "control: the proxy alone does not serve acme/late:1.0"
[ "$(manifest_status images/acme/late 2.0)" = "200" ] || fail "the pushed acme/late:2.0 is not served through images"
page_block claim-check server > "${WORK}/claim.sh"
claim="$(bash -e "${WORK}/claim.sh")"
[ "${claim}" = "404" ] || fail "images/acme/late:1.0 answered ${claim}, want 404: the claim did not hide Harbor's tag"
# The claim is per image name: the copied images were not touched.
[ "$(manifest_status images/acme/tools 1.0)" = "200" ] || fail "a push to acme/late changed acme/tools"
[ "$(manifest_status images/acme/app 1.1)" = "200" ] || fail "a push to acme/late changed acme/app"
echo "  acme/late:1.0 through images: ${claim}; the proxy alone: 200; acme/tools:1.0 and acme/app:1.1 unaffected: 200"
# The page: run the copy again, and the hidden tag is in internal beside the new one.
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/copy.sh" < /dev/null >/dev/null 2>&1 || fail "the page's copy fails when run again for acme/late"
PATH="${WORK}/bin/migrate:${PATH}" bash -e "${WORK}/verify.sh" < /dev/null > "${WORK}/verify2.out" 2>&1 || { cat "${WORK}/verify2.out" >&2; fail "the page's digest comparison failed after the second copy"; }
[ "$(grep -c '^ok ' "${WORK}/verify2.out")" = "4" ] || { cat "${WORK}/verify2.out" >&2; fail "the digest comparison did not report four matches"; }
[ "$(manifest_status images/acme/late 1.0)" = "200" ] || fail "images/acme/late:1.0 is still hidden after the second copy"
[ "$(manifest_status images/acme/late 2.0)" = "200" ] || fail "the second copy lost acme/late:2.0"
echo "  the second copy brought acme/late:1.0 in beside 2.0"

echo "== 11. Harbor off: pull by digest from hosted alone"
docker stop "${NC}" >/dev/null
docker rmi "${CSINDRI}/images/acme/app:1.0" >/dev/null 2>&1 || true
docker rmi "${CSINDRI}/images/acme/app@${D_APP10}" >/dev/null 2>&1 || true
page_block pull-digest > "${WORK}/pull-digest.sh"
DIGEST="${D_APP10}" bash -e "${WORK}/pull-digest.sh" > "${WORK}/pull-digest.out" 2>&1 || { cat "${WORK}/pull-digest.out" >&2; fail "the page's pull by digest failed with Harbor stopped"; }
grep -q "Digest: ${D_APP10}" "${WORK}/pull-digest.out" || { cat "${WORK}/pull-digest.out" >&2; fail "the digest pulled from hosted is not Harbor's"; }
echo "  stand-in stopped; images/acme/app@${D_APP10:0:19} pulled from hosted"

echo "== 12. secrets"
nlog_all="$(docker logs "${NC}" 2>&1)"
for secret in "${HARBOR_ROBOT_PASSWORD}" "${SEED_PASSWORD}" "${PULL_TOKEN}" "${PUSH_TOKEN}" "${SINDRI_ADMIN_TOKEN}"; do
  if grep -qF -- "${secret}" "${WORK}/sindri.log"; then fail "a secret is in Sindri's log"; fi
  if grep -qF -- "${secret}" <<<"${nlog_all}"; then fail "a secret is in the stand-in's log"; fi
done
echo "M10 harbor migration gate passed"
