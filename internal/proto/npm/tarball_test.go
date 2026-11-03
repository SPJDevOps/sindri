package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte // zero means a regular file
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: typeflag}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzipping: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func buildTarball(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	return gzipped(t, buildTar(t, entries...))
}

const plainManifest = `{"name":"foo","version":"1.0.0"}`

func TestInspectManifest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			name: "normal tarball",
			entries: []tarEntry{
				{name: "package/index.js", body: "module.exports = 1"},
				{name: "package/package.json", body: plainManifest},
				{name: "package/README.md", body: "# foo"},
			},
			want: plainManifest,
		},
		{
			name: "server fields are stripped",
			entries: []tarEntry{{name: "package/package.json", body: `{
				"name":"foo","version":"1.0.0",
				"dependencies":{"a":"1.0.0"},
				"dist":{"tarball":"http://evil/"},
				"_id":"evil","_npmUser":{"name":"evil"},"_custom":1,
				"deprecated":"evil","readme":"evil"
			}`}},
			want: `{"name":"foo","version":"1.0.0","dependencies":{"a":"1.0.0"}}`,
		},
		{
			name: "stripping sees through escapes and case",
			entries: []tarEntry{{name: "package/package.json",
				body: `{"name":"foo","\u0064ist":{},"DIST":{},"Readme":"x","\u005fid":"x"}`}},
			want: `{"name":"foo"}`,
		},
		{
			name: "only the top level is stripped",
			entries: []tarEntry{{name: "package/package.json",
				body: `{"name":"foo","config":{"dist":"keep","_id":"keep"}}`}},
			want: `{"name":"foo","config":{"dist":"keep","_id":"keep"}}`,
		},
		{
			name: "nested package.json does not count",
			entries: []tarEntry{
				{name: "package/package.json", body: plainManifest},
				{name: "package/nested/package.json", body: `{"name":"other"}`},
				{name: "package/node_modules/x/package.json", body: `{"name":"other"}`},
			},
			want: plainManifest,
		},
		{
			name: "manifest at the size bound",
			entries: []tarEntry{{name: "package/package.json",
				body: `{"k":"` + strings.Repeat("x", 1<<20-8) + `"}`}},
			want: `{"k":"` + strings.Repeat("x", 1<<20-8) + `"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(buildTarball(t, tc.entries...)))
			if err != nil {
				t.Fatalf("inspectManifest: %v", err)
			}
			var gotTree, wantTree map[string]json.RawMessage
			if err := json.Unmarshal(got, &gotTree); err != nil {
				t.Fatalf("result is not a JSON object: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantTree); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if diff := cmp.Diff(wantTree, gotTree); diff != "" {
				t.Errorf("manifest differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInspectManifest_Refused(t *testing.T) {
	t.Parallel()
	manifest := tarEntry{name: "package/package.json", body: plainManifest}
	cases := []struct {
		name    string
		entries []tarEntry
		reason  string
	}{
		{name: "no manifest", reason: "no package/package.json", entries: []tarEntry{{name: "package/index.js", body: "x"}}},
		{name: "empty archive", reason: "no package/package.json"},
		{name: "two manifests", reason: "more than one", entries: []tarEntry{manifest, manifest}},
		{name: "two manifests apart", reason: "more than one", entries: []tarEntry{
			manifest, {name: "package/index.js", body: "x"}, manifest,
		}},
		{name: "only a nested manifest", reason: "no package/package.json", entries: []tarEntry{
			{name: "package/nested/package.json", body: plainManifest},
		}},
		{name: "manifest over the size bound", reason: "too large", entries: []tarEntry{
			{name: "package/package.json", body: `{"k":"` + strings.Repeat("x", 1<<20-7) + `"}`},
		}},
		{name: "manifest not JSON", reason: "not a JSON object", entries: []tarEntry{{name: "package/package.json", body: "nope"}}},
		{name: "manifest empty", reason: "not a JSON object", entries: []tarEntry{{name: "package/package.json"}}},
		{name: "manifest an array", reason: "not a JSON object", entries: []tarEntry{{name: "package/package.json", body: "[]"}}},
		{name: "manifest null", reason: "not a JSON object", entries: []tarEntry{{name: "package/package.json", body: "null"}}},
		{name: "manifest with trailing data", reason: "not a JSON object", entries: []tarEntry{
			{name: "package/package.json", body: plainManifest + "x"},
		}},
		{name: "manifest a symlink", reason: "entry type", entries: []tarEntry{
			{name: "package/package.json", typeflag: tar.TypeSymlink},
		}},
		{name: "manifest an entry type that carries data", reason: "entry type", entries: []tarEntry{
			{name: "package/package.json", body: plainManifest, typeflag: tar.TypeCont},
		}},
		// An installer strips the first path component and lets the last entry
		// win, so each of these could replace the manifest Sindri read.
		{name: "another top directory after the manifest", reason: "install over", entries: []tarEntry{
			manifest, {name: "other/package.json", body: `{"scripts":{"postinstall":"evil"}}`},
		}},
		{name: "another top directory before the manifest", reason: "install over", entries: []tarEntry{
			{name: "other/package.json", body: `{"scripts":{"postinstall":"evil"}}`}, manifest,
		}},
		{name: "dot prefix", reason: "entry name", entries: []tarEntry{manifest, {name: "./package/package.json", body: "{}"}}},
		{name: "doubled slash", reason: "entry name", entries: []tarEntry{manifest, {name: "package//package.json", body: "{}"}}},
		{name: "leading slash", reason: "entry name", entries: []tarEntry{manifest, {name: "/package/package.json", body: "{}"}}},
		{name: "parent traversal", reason: "entry name", entries: []tarEntry{manifest, {name: "package/x/../package.json", body: "{}"}}},
		{name: "archive root", reason: "install over", entries: []tarEntry{manifest, {name: "package.json", body: "{}"}}},
		{name: "dot prefixed archive root", reason: "entry name", entries: []tarEntry{manifest, {name: "./package.json", body: "{}"}}},
		{name: "directory", reason: "install over", entries: []tarEntry{manifest, {name: "other/package.json/", typeflag: tar.TypeDir}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(buildTarball(t, tc.entries...)))
			if err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want it to give the reason %q", err, tc.reason)
			}
		})
	}
}

func TestInspectManifest_RefusesDamagedStreams(t *testing.T) {
	t.Parallel()
	good := buildTarball(t, tarEntry{name: "package/package.json", body: plainManifest})
	corrupt := bytes.Clone(good)
	corrupt[len(corrupt)-8] ^= 0xff // the gzip checksum, past the end of the tar

	cases := []struct {
		name    string
		data    []byte
		wantErr error
		reason  string
	}{
		{name: "not gzip", data: []byte("not a gzip stream"), wantErr: gzip.ErrHeader, reason: "gzip stream"},
		{name: "empty", data: nil, wantErr: io.EOF, reason: "gzip stream"},
		{name: "gzip of something other than tar", data: gzipped(t, bytes.Repeat([]byte("x"), 2048)), reason: "magic"},
		{name: "truncated gzip", data: good[:len(good)/2], reason: "end-of-archive"},
		{name: "bad gzip checksum", data: corrupt, wantErr: gzip.ErrChecksum, reason: "gzip stream"},
		{name: "garbage after the gzip stream", data: append(bytes.Clone(good), "garbage"...), reason: "after its gzip stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(tc.data))
			if err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
			if !errors.Is(err, errTarballRefused) {
				t.Errorf("err = %v, want errTarballRefused", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want it to give the reason %q", err, tc.reason)
			}
		})
	}
}

// A failing reader is the server's problem or the connection's, not a bad tarball.
func TestInspectManifest_ReadErrorsAreNotRefusals(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	good := buildTarball(t, tarEntry{name: "package/package.json", body: plainManifest})
	cases := []struct {
		name string
		r    io.Reader
	}{
		{name: "before the gzip header", r: iotest.ErrReader(boom)},
		{name: "inside the stream", r: io.MultiReader(bytes.NewReader(good[:len(good)/2]), iotest.ErrReader(boom))},
		{name: "after the stream", r: io.MultiReader(bytes.NewReader(good), iotest.ErrReader(boom))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := inspectManifest(tc.r)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap the reader's error", err)
			}
			if errors.Is(err, errTarballRefused) {
				t.Errorf("err = %v, want it not to be a refusal", err)
			}
		})
	}
}

// Repeating one flushed deflate block makes the real bound cheap to reach, the
// way a bomb would: about 2 MiB of input, in one gzip member, declaring a single
// entry larger than the bound.
func TestInspectManifest_GzipBombStopsAtTheBound(t *testing.T) {
	t.Parallel()
	header := rawHeader("package/blob", 1<<30+1<<20, '0', ustarMagic)

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(header); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	if _, err := zw.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	mark := compressed.Len()
	if _, err := zw.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	mebibytes := bytes.Clone(compressed.Bytes()[mark:])
	for range 1 << 10 {
		compressed.Write(mebibytes)
	}

	_, err := inspectManifest(&compressed)
	if !errors.Is(err, errUnpackedTooLarge) {
		t.Fatalf("err = %v, want errUnpackedTooLarge", err)
	}
	if !errors.Is(err, errTarballRefused) {
		t.Errorf("err = %v, want errTarballRefused too", err)
	}
}

func TestScanManifest_UnpackedBound(t *testing.T) {
	t.Parallel()
	manifest := tarEntry{name: "package/package.json", body: plainManifest}
	raw := buildTar(t, manifest, tarEntry{name: "package/blob", body: strings.Repeat("x", 4096)})
	size := int64(len(raw))
	// The end of the last entry, before the tar's end-of-archive blocks. A
	// limit that stops here reads as a clean end of archive unless it is checked.
	lastEntryEnd := size - 2*512

	withDuplicateAtBound := buildTar(t, manifest)
	withDuplicateAtBound = withDuplicateAtBound[:len(withDuplicateAtBound)-2*512]
	withDuplicateAtBound = append(withDuplicateAtBound, buildTar(t, manifest)...)

	cases := []struct {
		name    string
		raw     []byte
		bound   int64
		wantErr error
	}{
		{name: "whole stream at the bound", raw: raw, bound: size},
		{name: "one byte over", raw: raw, bound: size - 1, wantErr: errUnpackedTooLarge},
		{name: "bound at an entry boundary", raw: raw, bound: lastEntryEnd, wantErr: errUnpackedTooLarge},
		{name: "bound inside an entry", raw: raw, bound: 700, wantErr: errUnpackedTooLarge},
		{name: "bound before a duplicate cannot hide it", raw: withDuplicateAtBound,
			bound: int64(len(withDuplicateAtBound)) - 2*512 - 1024, wantErr: errUnpackedTooLarge},
		{name: "data after the end of the archive counts", raw: append(bytes.Clone(raw), make([]byte, 4096)...),
			bound: size, wantErr: errUnpackedTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := scanManifest(bytes.NewReader(gzipped(t, tc.raw)), tc.bound)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && !errors.Is(err, errTarballRefused) {
				t.Errorf("err = %v, want errTarballRefused too", err)
			}
		})
	}
}

func TestHashTarball(t *testing.T) {
	t.Parallel()
	input := []byte("fixed tarball bytes\x00\x01\x02")

	sum256 := sha256.Sum256(input)
	sum512 := sha512.Sum512(input)
	sum1 := sha1.Sum(input)

	got, err := hashTarball(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("hashTarball: %v", err)
	}
	if got.Size != int64(len(input)) {
		t.Errorf("Size = %d, want %d", got.Size, len(input))
	}
	if want := "sha256:" + hex.EncodeToString(sum256[:]); got.SHA256.String() != want {
		t.Errorf("SHA256 = %s, want %s", got.SHA256, want)
	}
	if want := "sha512-" + base64.StdEncoding.EncodeToString(sum512[:]); got.Integrity != want {
		t.Errorf("Integrity = %s, want %s", got.Integrity, want)
	}
	if want := hex.EncodeToString(sum1[:]); got.Shasum != want {
		t.Errorf("Shasum = %s, want %s", got.Shasum, want)
	}
}

func TestHashTarball_PropagatesReadErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	if _, err := hashTarball(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the reader's error", err)
	}
}

// Ordinary numbers must never trip the bound. This is the shape of a real manifest,
// with the numbers packages do put in one.
func TestInspectManifest_RealLookingManifestWithOrdinaryNumbersIsKept(t *testing.T) {
	t.Parallel()
	const manifest = `{
  "name": "@acme/ui",
  "version": "1.2.3",
  "main": "index.js",
  "license": "MIT",
  "engines": {"node": ">=18"},
  "dependencies": {"left-pad": "^1.3.0"},
  "config": {"port": 8080, "ratio": 0.75, "offset": -12, "timeoutMs": 3e4, "epsilon": 2.5e-7, "limit": 1E+21},
  "sideEffects": false,
  "files": ["dist", "lib"]
}`
	got, err := inspectManifest(bytes.NewReader(buildTarball(t, tarEntry{name: manifestEntry, body: manifest})))
	if err != nil {
		t.Fatalf("inspectManifest: %v", err)
	}
	var want, have map[string]any
	if err := json.Unmarshal([]byte(manifest), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal(have, want) {
		t.Fatalf("manifest changed:\n%s", cmp.Diff(want, have))
	}
}

func TestInspectManifest_RefusesAHugeExponentWithFixedText(t *testing.T) {
	t.Parallel()
	const manifest = `{"name":"foo","version":"1.0.0","secret-marker":1e1000}`
	got, err := inspectManifest(bytes.NewReader(buildTarball(t, tarEntry{name: manifestEntry, body: manifest})))
	if !errors.Is(err, errTarballRefused) {
		t.Fatalf("inspectManifest = %s, %v, want a refusal", got, err)
	}
	if strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "1e1000") {
		t.Fatalf("refusal quotes the manifest: %v", err)
	}
}

func TestManifestStorable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{name: "plain", doc: `{"a":"b","n":[1,-2.5,3e2],"t":true,"z":null}`, want: true},
		{name: "ordinary escapes", doc: `{"a":"\\\"\n\u0041\/"}`, want: true},
		{name: "escaped backslash then u0000 text", doc: `{"a":"\\u0000"}`, want: true},
		{name: "surrogate pair", doc: `{"a":"\ud83d\ude00"}`, want: true},
		{name: "uppercase hex", doc: `{"a":"\uD83D\uDE00"}`, want: true},
		{name: "number inside a string is text", doc: `{"a":"1e999999"}`, want: true},
		{name: "number at the end of the document", doc: `1e64`, want: true},
		{name: "largest exponent", doc: `{"a":1e64}`, want: true},
		{name: "largest exponent, signed", doc: `{"a":1E+64}`, want: true},
		{name: "smallest exponent", doc: `{"a":-1.5e-64}`, want: true},
		{name: "escaped NUL", doc: `{"a":"\u0000"}`},
		{name: "escaped NUL after an escaped backslash", doc: `{"a":"\\\u0000"}`},
		{name: "escaped NUL in a key", doc: `{"\u0000":1}`},
		{name: "invalid UTF-8", doc: "{\"a\":\"\xff\"}"},
		{name: "high surrogate at the end", doc: `{"a":"\ud83d"}`},
		{name: "high surrogate before text", doc: `{"a":"\ud83dx"}`},
		{name: "high surrogate before a plain escape", doc: `{"a":"\ud83d\n"}`},
		{name: "high surrogate before a non-surrogate", doc: `{"a":"\ud83d\u0041"}`},
		{name: "two high surrogates", doc: `{"a":"\ud83d\ud83d"}`},
		{name: "low surrogate first", doc: `{"a":"\ude00\ud83d"}`},
		{name: "exponent just too large", doc: `{"a":1e65}`},
		{name: "exponent just too small", doc: `{"a":1E-65}`},
		{name: "exponent that jsonb would write out in full", doc: `{"a":1e1000}`},
		{name: "exponent at the end of the document", doc: `1e65`},
		{name: "exponent past an int", doc: `{"a":1e99999999999999999999}`},
		{name: "too many digits", doc: `{"a":` + strings.Repeat("1", maxManifestNumber+1) + `}`},
		{name: "too deep", doc: strings.Repeat("[", maxManifestDepth+1) + strings.Repeat("]", maxManifestDepth+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := manifestStorable([]byte(tc.doc)); got != tc.want {
				t.Fatalf("manifestStorable(%q) = %v, want %v", tc.doc, got, tc.want)
			}
		})
	}
}
