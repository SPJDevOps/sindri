#!/usr/bin/env bash
# SIN-51: compare Sindri's npm tarball parser with what npm, pnpm and yarn install.
#
# Local only: the clients install tarballs from files, with --ignore-scripts, into
# temp directories. The one network use is fetching the pinned pnpm and yarn from
# npmjs into a temp prefix. Nothing is installed globally and nothing is published.
# Uses the npm on PATH.
set -euo pipefail

PNPM_VERSION="${PNPM_VERSION:-10.34.9}"
YARN_VERSION="${YARN_VERSION:-1.22.22}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sindri-npm-differential.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

TOOLS="${WORK}/tools"
mkdir -p "${TOOLS}" "${WORK}/home"

# Every client gets a home inside the temp directory, so no cache or config is
# written outside it.
export HOME="${WORK}/home"
export XDG_CACHE_HOME="${WORK}/home/.cache"
export XDG_DATA_HOME="${WORK}/home/.local/share"
export XDG_CONFIG_HOME="${WORK}/home/.config"
export npm_config_cache="${WORK}/home/.npm"
export npm_config_userconfig="${WORK}/home/.npmrc"
export npm_config_globalconfig="${WORK}/home/.npmrc-global"
export npm_config_update_notifier=false
export npm_config_manage_package_manager_versions=false

echo "npm   $(npm --version)  ($(command -v npm))"
npm install --prefix "${TOOLS}" --ignore-scripts --no-audit --no-fund --no-package-lock \
  "pnpm@${PNPM_VERSION}" "yarn@${YARN_VERSION}" >/dev/null
echo "pnpm  $("${TOOLS}/node_modules/.bin/pnpm" --version)"
echo "yarn  $("${TOOLS}/node_modules/.bin/yarn" --version)"
echo

export SINDRI_DIFF_NPM
SINDRI_DIFF_NPM="$(command -v npm)"
export SINDRI_DIFF_PNPM="${TOOLS}/node_modules/.bin/pnpm"
export SINDRI_DIFF_YARN="${TOOLS}/node_modules/.bin/yarn"
export SINDRI_DIFF_WORK="${WORK}/runs"
mkdir -p "${SINDRI_DIFF_WORK}"

cd "${ROOT}"
go test -tags differential -count=1 -v -parallel 4 -run '^TestDifferential_' ./internal/proto/npm/
