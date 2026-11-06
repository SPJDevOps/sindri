#!/usr/bin/env bash
# Install the pinned docs toolchain, mdBook and lychee, into a directory.
#
#   ./scripts/install-docs-tools.sh [dir]   # default: $(go env GOBIN) or $(go env GOPATH)/bin
#
# Both are Rust binaries with no `go install` path, so this downloads their
# release archives and checks each against a sha256 pinned here before
# unpacking. mdBook publishes no checksums; its sums were computed from the
# release assets when the version was pinned. lychee's match the .sha256
# files it publishes. To bump a version, update the version and all four sums
# together (ADR 26).
set -euo pipefail

MDBOOK_VERSION="v0.5.4"
LYCHEE_VERSION="v0.24.2"

DEST="${1:-$(go env GOBIN)}"
DEST="${DEST:-$(go env GOPATH)/bin}"
mkdir -p "${DEST}"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  TRIPLE="aarch64-apple-darwin" ;;
  Darwin-x86_64) TRIPLE="x86_64-apple-darwin" ;;
  Linux-x86_64)  TRIPLE="x86_64-unknown-linux-musl" ;;
  Linux-aarch64) TRIPLE="aarch64-unknown-linux-musl" ;;
  *) echo "no pinned docs tools for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

sum() {
  case "$1" in
    mdbook-aarch64-apple-darwin)       echo 03e8a6d8b13a2971e0b3280affd03b388373c1485e26f73407c3a76b0b1838df ;;
    mdbook-x86_64-apple-darwin)        echo a47d7bf0d5d670cff9ee6cce95537cbeb62dc10704d9e7131ffbd13e2b59a5de ;;
    mdbook-x86_64-unknown-linux-musl)  echo 5222beabd3e37dc5be0d18ff99b79058469354db5c220153a1b92db5ba12be89 ;;
    mdbook-aarch64-unknown-linux-musl) echo 753e5c5c363ee8a56972344dcf91466f005a51db84a7aeffe427ae3ef83d6d44 ;;
    lychee-aarch64-apple-darwin)       echo c9d3740ea2d891854d37116c9fba840f37b6e7c89d330e7db84ac333631c4977 ;;
    lychee-x86_64-apple-darwin)        echo 887503a9cff667d322b8d0892b40bf49976eb9507af8483220a3706cdad55978 ;;
    lychee-x86_64-unknown-linux-musl)  echo 73657a111819a30c47c08352896796f23d64e4eb2b3ed39b6d32149241566fc5 ;;
    lychee-aarch64-unknown-linux-musl) echo 5d0b0e3aeab240f41920c633a6eaf97599be6eedda034b36e858ede7dba5e535 ;;
  esac
}

if command -v sha256sum >/dev/null; then
  SHA256="sha256sum"
else
  SHA256="shasum -a 256"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# fetch <tool> <url> <path of the binary inside the archive>
fetch() {
  local tool="$1" url="$2" member="$3"
  local archive="${WORK}/${tool}.tar.gz"
  curl -fsSL --retry 3 -o "${archive}" "${url}"
  echo "$(sum "${tool}-${TRIPLE}")  ${archive}" | ${SHA256} -c --quiet - \
    || { echo "${tool}: checksum mismatch for ${url}" >&2; exit 1; }
  tar -xzf "${archive}" -C "${WORK}" "${member}"
  install -m 0755 "${WORK}/${member}" "${DEST}/${tool}"
  echo "installed ${tool} into ${DEST}"
}

fetch mdbook \
  "https://github.com/rust-lang/mdBook/releases/download/${MDBOOK_VERSION}/mdbook-${MDBOOK_VERSION}-${TRIPLE}.tar.gz" \
  mdbook
fetch lychee \
  "https://github.com/lycheeverse/lychee/releases/download/lychee-${LYCHEE_VERSION}/lychee-${TRIPLE}.tar.gz" \
  "lychee-${TRIPLE}/lychee"
