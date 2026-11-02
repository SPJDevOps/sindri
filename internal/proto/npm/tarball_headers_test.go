package npm

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// tar.Writer will not emit the header shapes these tests need, so they are built
// by hand. The two parsers that matter here are Go's, which Sindri reads a
// tarball with, and node-tar, which installs it.

const (
	readBySindri     = `{"name":"t-pkg","version":"1.0.0","description":"READ-BY-SINDRI"}`
	installedVariant = `{"name":"t-pkg","version":"1.0.0","description":"INSTALLED-VARIANT"}`

	ustarMagic = "ustar\x0000"
	gnuMagic   = "ustar  \x00"
)

func rawHeader(name string, size int, typeflag byte, magic string) []byte {
	b := make([]byte, 512)
	copy(b[0:], name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	b[156] = typeflag
	copy(b[257:], magic)
	sealHeader(b)
	return b
}

func sealHeader(b []byte) {
	copy(b[148:], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
}

func rawEntry(name, body string, typeflag byte) []byte {
	entry := append(rawHeader(name, len(body), typeflag, ustarMagic), body...)
	if extra := len(body) % 512; extra != 0 {
		entry = append(entry, make([]byte, 512-extra)...)
	}
	return entry
}

// paxRecords encodes "key", "value" pairs as a pax extended header body, whose
// records start with their own length in bytes.
func paxRecords(pairs ...string) string {
	var b strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		rest := " " + pairs[i] + "=" + pairs[i+1] + "\n"
		n := len(rest)
		for len(fmt.Sprint(n))+len(rest) != n {
			n = len(fmt.Sprint(n)) + len(rest)
		}
		fmt.Fprintf(&b, "%d%s", n, rest)
	}
	return b.String()
}

func rawTarball(t *testing.T, entries ...[]byte) []byte {
	t.Helper()
	raw := bytes.Join(entries, nil)
	return gzipped(t, append(raw, make([]byte, 2*512)...))
}

func TestInspectManifest_RefusesHeadersParsersDisagreeOn(t *testing.T) {
	t.Parallel()
	benign := rawEntry(manifestEntry, readBySindri, tar.TypeReg)
	smuggled := rawEntry("package/also-harmless.js", installedVariant, tar.TypeReg)
	size := fmt.Sprint(len(installedVariant))

	cases := []struct {
		name    string
		entries [][]byte
	}{
		{
			// Go renames the entry to GNU.sparse.name; node-tar honours path.
			name: "sparse name hides a pax path",
			entries: [][]byte{benign, rawEntry("PaxHeaders/x", paxRecords(
				"path", manifestEntry,
				"GNU.sparse.major", "0", "GNU.sparse.minor", "1",
				"GNU.sparse.numblocks", "1", "GNU.sparse.map", "0,"+size,
				"GNU.sparse.realsize", size, "GNU.sparse.name", "package/harmless.js",
			), tar.TypeXHeader), smuggled},
		},
		{
			// Go lets the long name win; node-tar honours path.
			name: "long name then pax path",
			entries: [][]byte{benign,
				rawEntry("././@LongLink", "package/harmless.js\x00", tar.TypeGNULongName),
				rawEntry("PaxHeaders/x", paxRecords("path", manifestEntry), tar.TypeXHeader),
				smuggled},
		},
		{
			name: "pax path then long name",
			entries: [][]byte{benign,
				rawEntry("PaxHeaders/x", paxRecords("path", manifestEntry), tar.TypeXHeader),
				rawEntry("././@LongLink", "package/harmless.js\x00", tar.TypeGNULongName),
				smuggled},
		},
		{
			name: "long name with an unrelated pax path",
			entries: [][]byte{benign,
				rawEntry("././@LongLink", "package/harmless.js\x00", tar.TypeGNULongName),
				rawEntry("PaxHeaders/x", paxRecords("path", "package/other.js"), tar.TypeXHeader),
				smuggled},
		},
		{
			name: "one sparse record",
			entries: [][]byte{benign,
				rawEntry("PaxHeaders/x", paxRecords("GNU.sparse.major", "1"), tar.TypeXHeader),
				smuggled},
		},
		{
			name: "old GNU sparse type",
			entries: [][]byte{benign,
				rawHeader("package/harmless.js", 0, tar.TypeGNUSparse, gnuMagic)},
		},
		{
			// Go reports a global header as an entry named by its path record.
			name: "global pax path",
			entries: [][]byte{
				rawEntry("pax_global_header", paxRecords("path", manifestEntry), tar.TypeXGlobalHeader),
				benign, smuggled},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := inspectManifest(bytes.NewReader(rawTarball(t, tc.entries...))); err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
		})
	}
}

// What real tools write for long paths has to keep working: the refusals above
// only apply when the headers disagree.
func TestInspectManifest_AcceptsLongPathHeaders(t *testing.T) {
	t.Parallel()
	benign := rawEntry(manifestEntry, readBySindri, tar.TypeReg)

	dir := "package/" + strings.Repeat("d", 90) + "/"
	overUstar := "package/" + strings.Repeat("d/", 150) + "f.js"

	withPrefix := rawHeader(strings.Repeat("f", 40)+".js", 1, tar.TypeReg, ustarMagic)
	copy(withPrefix[345:], strings.TrimSuffix(dir, "/"))
	sealHeader(withPrefix)

	cases := []struct {
		name    string
		entries [][]byte
	}{
		{name: "ustar prefix", entries: [][]byte{
			benign, withPrefix, make([]byte, 512),
		}},
		{name: "pax path over a truncated name", entries: [][]byte{
			benign,
			rawEntry("PaxHeaders/x", paxRecords("path", overUstar), tar.TypeXHeader),
			rawEntry(overUstar[:100], "x", tar.TypeReg),
		}},
		{name: "pax path naming the manifest", entries: [][]byte{
			rawEntry("PaxHeaders/x", paxRecords("path", manifestEntry), tar.TypeXHeader),
			benign,
		}},
		{name: "gnu long name", entries: [][]byte{
			benign,
			rawEntry("././@LongLink", overUstar+"\x00", tar.TypeGNULongName),
			rawEntry(overUstar[:100], "x", tar.TypeReg),
		}},
		{name: "global header without a path", entries: [][]byte{
			rawEntry("pax_global_header", paxRecords("comment", "0123abcd"), tar.TypeXGlobalHeader),
			benign,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(rawTarball(t, tc.entries...)))
			if err != nil {
				t.Fatalf("inspectManifest: %v", err)
			}
			var m struct {
				Description string `json:"description"`
			}
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			if m.Description != "READ-BY-SINDRI" {
				t.Errorf("description = %q, want the manifest's", m.Description)
			}
		})
	}
}

func TestInspectManifest_LongPathsFromTarWriter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{name: "fits a ustar prefix", path: "package/" + strings.Repeat("d", 90) + "/" + strings.Repeat("f", 40) + ".js"},
		{name: "needs a pax record", path: "package/" + strings.Repeat("d/", 150) + "f.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tarball := buildTarball(t,
				tarEntry{name: manifestEntry, body: plainManifest},
				tarEntry{name: tc.path, body: "x"})
			if _, err := inspectManifest(bytes.NewReader(tarball)); err != nil {
				t.Fatalf("inspectManifest: %v", err)
			}
		})
	}
}

func TestInspectManifest_RefusesManifestAliases(t *testing.T) {
	t.Parallel()
	manifest := tarEntry{name: manifestEntry, body: plainManifest}
	alias := func(name string) tarEntry { return tarEntry{name: name, body: plainManifest} }

	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{name: "uppercase base name alone", entries: []tarEntry{alias("package/PACKAGE.JSON")}},
		{name: "uppercase base name after the manifest", entries: []tarEntry{manifest, alias("package/PACKAGE.JSON")}},
		{name: "uppercase base name before the manifest", entries: []tarEntry{alias("package/PACKAGE.JSON"), manifest}},
		{name: "mixed case base name", entries: []tarEntry{manifest, alias("package/Package.json")}},
		{name: "uppercase directory alone", entries: []tarEntry{alias("PACKAGE/package.json")}},
		{name: "uppercase directory after the manifest", entries: []tarEntry{manifest, alias("PACKAGE/package.json")}},
		{name: "uppercase archive root", entries: []tarEntry{manifest, alias("PACKAGE.JSON")}},
		{name: "backslash alone", entries: []tarEntry{alias(`package\package.json`)}},
		{name: "backslash after the manifest", entries: []tarEntry{manifest, alias(`package\package.json`)}},
		{name: "backslash and uppercase", entries: []tarEntry{manifest, alias(`package\PACKAGE.JSON`)}},
		{name: "backslash traversal", entries: []tarEntry{manifest, alias(`package\x\..\package.json`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := inspectManifest(bytes.NewReader(buildTarball(t, tc.entries...))); err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
		})
	}

	t.Run("a nested variant is not an alias", func(t *testing.T) {
		t.Parallel()
		tarball := buildTarball(t, manifest, alias("package/nested/PACKAGE.JSON"), alias(`package\nested\package.json`))
		if _, err := inspectManifest(bytes.NewReader(tarball)); err != nil {
			t.Fatalf("inspectManifest: %v", err)
		}
	})
}
