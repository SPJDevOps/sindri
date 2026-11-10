#!/usr/bin/env bash
# M10 docs gate: the docs site builds with no broken links, the configuration
# reference matches the code, and every command a docs page shows runs.
#
#   ./scripts/m10-docs-gate.sh
#
# Each page in docs/ that shows commands names what runs them, in a comment
# such as <!-- commands checked by: scripts/upgrade-gate.sh --> or
# <!-- commands checked by: TestDocs_ConfigurationCommandsRun -->.
# TestDocs_EveryPageWithCommandsNamesItsCheck fails on a page that names none.
# This gate reads those comments and runs every check they name, so a new page
# with a new check is covered here without editing this script.
#
# The scripts it runs need Docker.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

echo "== the docs site builds, and no internal link or anchor is broken"
make docs

echo "== the reference matches internal/config, and every page names its check"
go test -count=1 ./internal/config -run 'TestReference_'
go test -count=1 ./cmd/sindri -run 'TestDocs_'

checks="$(grep -rhoE '<!-- commands checked by: [^ ]+ -->' --include='*.md' docs \
  | sed -E 's/<!-- commands checked by: ([^ ]+) -->/\1/' | sort -u)"
test -n "${checks}" || { echo "no docs page names a check" >&2; exit 1; }

# The pip and npm pages' gates run ./bin/sindri against the development Postgres.
echo "== building the binary and starting the development stack"
make build up
for _ in $(seq 1 60); do
  docker exec sindri-dev-postgres-1 pg_isready -q -U sindri -d sindri && break
  sleep 1
done

for check in ${checks}; do
  case "${check}" in
    scripts/*)
      echo "== ${check}"
      "./${check}"
      ;;
    Test*)
      echo "== ${check}"
      out="$(go test -count=1 -run "^${check}\$" -v ./... 2>&1)" || { echo "${out}" >&2; exit 1; }
      grep -q -- "--- PASS: ${check} " <<<"${out}" || { echo "${check} did not run" >&2; exit 1; }
      ;;
    *)
      echo "a docs page names ${check}, which is neither a script nor a test" >&2
      exit 1
      ;;
  esac
done

echo "M10 docs gate passed: $(wc -w <<<"${checks}" | tr -d ' ') checks named by the docs pages ran"
