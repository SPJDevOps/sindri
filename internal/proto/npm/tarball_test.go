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
	}{
		{name: "no manifest", entries: []tarEntry{{name: "package/index.js", body: "x"}}},
		{name: "empty archive"},
		{name: "two manifests", entries: []tarEntry{manifest, manifest}},
		{name: "two manifests apart", entries: []tarEntry{
			manifest, {name: "package/index.js", body: "x"}, manifest,
		}},
		{name: "only a nested manifest", entries: []tarEntry{
			{name: "package/nested/package.json", body: plainManifest},
		}},
		{name: "manifest over the size bound", entries: []tarEntry{
			{name: "package/package.json", body: `{"k":"` + strings.Repeat("x", 1<<20-7) + `"}`},
		}},
		{name: "manifest not JSON", entries: []tarEntry{{name: "package/package.json", body: "nope"}}},
		{name: "manifest empty", entries: []tarEntry{{name: "package/package.json"}}},
		{name: "manifest an array", entries: []tarEntry{{name: "package/package.json", body: "[]"}}},
		{name: "manifest null", entries: []tarEntry{{name: "package/package.json", body: "null"}}},
		{name: "manifest with trailing data", entries: []tarEntry{
			{name: "package/package.json", body: plainManifest + "x"},
		}},
		{name: "manifest a symlink", entries: []tarEntry{
			{name: "package/package.json", typeflag: tar.TypeSymlink},
		}},
		{name: "manifest an entry type that carries data", entries: []tarEntry{
			{name: "package/package.json", body: plainManifest, typeflag: tar.TypeCont},
		}},
		// An installer strips the first path component and lets the last entry
		// win, so each of these could replace the manifest Sindri read.
		{name: "another top directory after the manifest", entries: []tarEntry{
			manifest, {name: "other/package.json", body: `{"scripts":{"postinstall":"evil"}}`},
		}},
		{name: "another top directory before the manifest", entries: []tarEntry{
			{name: "other/package.json", body: `{"scripts":{"postinstall":"evil"}}`}, manifest,
		}},
		{name: "dot prefix", entries: []tarEntry{manifest, {name: "./package/package.json", body: "{}"}}},
		{name: "doubled slash", entries: []tarEntry{manifest, {name: "package//package.json", body: "{}"}}},
		{name: "leading slash", entries: []tarEntry{manifest, {name: "/package/package.json", body: "{}"}}},
		{name: "parent traversal", entries: []tarEntry{manifest, {name: "package/x/../package.json", body: "{}"}}},
		{name: "archive root", entries: []tarEntry{manifest, {name: "package.json", body: "{}"}}},
		{name: "dot prefixed archive root", entries: []tarEntry{manifest, {name: "./package.json", body: "{}"}}},
		{name: "directory", entries: []tarEntry{manifest, {name: "other/package.json/", typeflag: tar.TypeDir}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := inspectManifest(bytes.NewReader(buildTarball(t, tc.entries...))); err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
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
		name string
		data []byte
	}{
		{name: "not gzip", data: []byte("not a gzip stream")},
		{name: "empty", data: nil},
		{name: "gzip of something other than tar", data: gzipped(t, bytes.Repeat([]byte("x"), 2048))},
		{name: "truncated gzip", data: good[:len(good)/2]},
		{name: "bad gzip checksum", data: corrupt},
		{name: "garbage after the gzip stream", data: append(bytes.Clone(good), "garbage"...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := inspectManifest(bytes.NewReader(tc.data)); err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
		})
	}
}

func TestInspectManifest_PropagatesReadErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	if _, err := inspectManifest(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the reader's error", err)
	}
}

// Concatenated gzip members make the real bound cheap to reach, the way a bomb
// would: about 2 MiB of input declaring a single entry larger than the bound.
func TestInspectManifest_GzipBombStopsAtTheBound(t *testing.T) {
	t.Parallel()
	var header bytes.Buffer
	tw := tar.NewWriter(&header)
	if err := tw.WriteHeader(&tar.Header{Name: "package/blob", Mode: 0o644, Size: 1<<30 + 1<<20, Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}

	var bomb bytes.Buffer
	bomb.Write(gzipped(t, header.Bytes()))
	mebibyte := gzipped(t, make([]byte, 1<<20))
	for range 1<<10 + 2 {
		bomb.Write(mebibyte)
	}

	if _, err := inspectManifest(&bomb); !errors.Is(err, errUnpackedTooLarge) {
		t.Fatalf("err = %v, want errUnpackedTooLarge", err)
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
