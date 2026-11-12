#!/bin/sh
# A stand-in for the trivy executable in unit tests. It copies a fixture to the
# --output path. TRIVY_STUB_FAIL=1 makes it fail; TRIVY_STUB_ARGS names a file
# that receives one line per invocation with the argv. TRIVY_STUB_ENV names a
# file that receives the working directory and the environment of each
# invocation. TRIVY_STUB_REPORT_FILE names a file holding the name of a fixture
# in this directory; while it exists, image scans emit that fixture, which lets a
# test simulate a newer vulnerability database between two scans.
dir=$(cd "$(dirname "$0")" && pwd)

if [ -n "$TRIVY_STUB_ARGS" ]; then
	echo "$*" >>"$TRIVY_STUB_ARGS"
fi

if [ -n "$TRIVY_STUB_ENV" ]; then
	{
		echo "CWD=$(pwd -P)"
		env
	} >>"$TRIVY_STUB_ENV"
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

if [ "$1" = "image" ] && [ -n "$TRIVY_STUB_REPORT_FILE" ] && [ -s "$TRIVY_STUB_REPORT_FILE" ]; then
	fixture=$(cat "$TRIVY_STUB_REPORT_FILE")
fi

if [ -z "$out" ]; then
	echo "stub: missing --output" >&2
	exit 2
fi
cp "$dir/$fixture" "$out"
