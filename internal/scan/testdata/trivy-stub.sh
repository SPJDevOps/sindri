#!/bin/sh
# A stand-in for the trivy executable in unit tests. It copies a fixture to the
# --output path. TRIVY_STUB_FAIL=1 makes it fail; TRIVY_STUB_ARGS names a file
# that receives one line per invocation with the argv.
dir=$(cd "$(dirname "$0")" && pwd)

if [ -n "$TRIVY_STUB_ARGS" ]; then
	echo "$*" >>"$TRIVY_STUB_ARGS"
fi

if [ "$1" = "--version" ]; then
	cat "$dir/version.json"
	exit 0
fi

if [ -n "$TRIVY_STUB_FAIL" ]; then
	echo "FATAL stub failure: vulnerability database unavailable" >&2
	exit 1
fi

out=""
prev=""
for a in "$@"; do
	if [ "$prev" = "--output" ]; then
		out=$a
	fi
	prev=$a
done

case "$1" in
image) fixture=report-image.json ;;
rootfs) fixture=report-package.json ;;
convert) fixture=sbom.cdx.json ;;
*)
	echo "stub: unknown subcommand $1" >&2
	exit 2
	;;
esac

if [ -z "$out" ]; then
	echo "stub: missing --output" >&2
	exit 2
fi
cp "$dir/$fixture" "$out"
