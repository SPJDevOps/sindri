package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tar.Writer will not emit most of these shapes, so they are built by hand. Each
// refusal below was either reproduced against npm or is a neighbour of one that
// was: inspectManifest read one package.json and npm installed another.

const (
	readBySindri     = `{"name":"t-pkg","version":"1.0.0","description":"READ-BY-SINDRI"}`
	installedVariant = `{"name":"t-pkg","version":"1.0.0","description":"INSTALLED-VARIANT"}`

	ustarMagic = "ustar\x0000"
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
	copy(b[329:], "0000000\x00")
	copy(b[337:], "0000000\x00")
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

// patchedHeader changes a valid header and seals it again, so the change is the
// only thing wrong with it.
func patchedHeader(name string, size int, typeflag byte, patch func(b []byte)) []byte {
	b := rawHeader(name, size, typeflag, ustarMagic)
	patch(b)
	sealHeader(b)
	return b
}

func padded(body string) []byte {
	b := []byte(body)
	if extra := len(b) % 512; extra != 0 {
		b = append(b, make([]byte, 512-extra)...)
	}
	return b
}

func rawEntry(name, body string, typeflag byte) []byte {
	return append(rawHeader(name, len(body), typeflag, ustarMagic), padded(body)...)
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

// prefixedEntry stores a name across the ustar prefix and name fields.
func prefixedEntry(prefix, name, body string) []byte {
	b := rawHeader(name, len(body), '0', ustarMagic)
	copy(b[345:], prefix)
	sealHeader(b)
	return append(b, padded(body)...)
}

// checksumOfEightDigits has a correct checksum with no terminator, so its last
// digit sits against the digit type flag, and a parser reading on from the field
// sees a different number.
func checksumOfEightDigits(name string) []byte {
	b := rawHeader(name, 0, '0', ustarMagic)
	copy(b[148:], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%08o", sum))
	return b
}

func paxEntry(body string) []byte { return rawEntry("PaxHeader/x", body, 'x') }

func endOfArchive() []byte { return make([]byte, 2*512) }

func rawTarball(t *testing.T, entries ...[]byte) []byte {
	t.Helper()
	return gzipped(t, append(bytes.Join(entries, nil), endOfArchive()...))
}

func TestInspectManifest_RefusesShapesOutsideTheGrammar(t *testing.T) {
	t.Parallel()
	benign := rawEntry(manifestEntry, readBySindri, '0')
	hidden := rawEntry(manifestEntry, installedVariant, '0')
	smuggled := rawEntry("package/also-harmless.js", installedVariant, '0')
	size := fmt.Sprint(len(installedVariant))
	hiddenBody := hidden[512:]

	withVersionXX := rawHeader(manifestEntry, len(installedVariant), '0', "ustar\x00XX")
	copy(withVersionXX[345:], "zzz/yyy")
	sealHeader(withVersionXX)

	// Valid only if the checksum is summed as signed bytes.
	signedSum := rawHeader("package/z.js", len(hidden), '0', ustarMagic)
	signedSum[500] = 0xff
	copy(signedSum[148:], "        ")
	sum := 0
	for _, c := range signedSum {
		sum += int(int8(c))
	}
	copy(signedSum[148:], fmt.Sprintf("%06o\x00 ", sum))

	field := func(off int, value string) func([]byte) {
		return func(b []byte) {
			copy(b[off:], value)
		}
	}
	emptyField := func(off, n int) func([]byte) {
		return func(b []byte) {
			clear(b[off : off+n])
		}
	}
	reg := func(patch func([]byte)) []byte { return patchedHeader("package/z.js", 0, '0', patch) }

	cases := []struct {
		name    string
		entries [][]byte
		reason  string
	}{
		// The shapes reproduced against npm 10.9.9.
		{name: "s01 type N carrying a path", reason: "entry type",
			entries: [][]byte{benign, rawEntry("x", manifestEntry+"\x00", 'N'), smuggled}},
		{name: "s02 type X with a path", reason: "entry type",
			entries: [][]byte{benign, rawEntry("x", paxRecords("path", manifestEntry), 'X'), smuggled}},
		{name: "s03 x then x", reason: "two pax headers",
			entries: [][]byte{benign, paxEntry(paxRecords("path", manifestEntry)), paxEntry(paxRecords("comment", "1")), smuggled}},
		{name: "s04 x then g", reason: "entry type",
			entries: [][]byte{benign, paxEntry(paxRecords("path", manifestEntry)), rawEntry("g", paxRecords("comment", "1"), 'g'), smuggled}},
		{name: "s05 ustar version XX with a prefix", reason: "magic",
			entries: [][]byte{benign, withVersionXX, hiddenBody}},
		{name: "s06 regular entry with a trailing slash and a body", reason: "trailing slash",
			entries: [][]byte{benign, rawHeader("package/d/", len(hidden), '0', ustarMagic), hidden}},
		{name: "s07 symlink with a size", reason: "entry type",
			entries: [][]byte{benign, patchedHeader("package/l", 512, '2', field(157, "x")), rawHeader("package/z.js", len(hidden), '0', ustarMagic), hidden}},
		{name: "s08 signed checksum", reason: "checksum",
			entries: [][]byte{benign, signedSum, hidden}},
		{name: "s09 linkname on a regular entry", reason: "linkname",
			entries: [][]byte{benign, patchedHeader("package/z.js", len(hidden), '0', field(157, "x")), hidden}},
		{name: "s10 empty name", reason: "name",
			entries: [][]byte{benign, rawHeader("", len(hidden), '0', ustarMagic), hidden}},
		{name: "s11 g with a size record", reason: "entry type",
			entries: [][]byte{benign, rawEntry("g", paxRecords("size", size), 'g'), rawHeader("package/z.js", 512+len(hidden), '0', ustarMagic), make([]byte, 512), hidden}},
		{name: "s12 size with leading NULs", reason: "numeric field",
			entries: [][]byte{benign, reg(field(124, "\x00\x00\x00\x00\x00\x00\x002000\x00")), hidden}},
		{name: "s13 pax path with a dot segment", reason: "name",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "./package/package.json")), smuggled}},
		{name: "s14 pax path with a doubled slash", reason: "name",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package//package.json")), smuggled}},
		{name: "s15 pax path naming the manifest on a directory", reason: "more than one",
			entries: [][]byte{benign, paxEntry(paxRecords("path", manifestEntry)), rawHeader("zzz", 0, '5', ustarMagic), smuggled}},
		{name: "s16 pax linkpath", reason: "pax key",
			entries: [][]byte{benign, paxEntry(paxRecords("linkpath", manifestEntry)), smuggled}},
		{name: "s17 K then regular", reason: "entry type",
			entries: [][]byte{benign, rawEntry("x", manifestEntry+"\x00", 'K'), smuggled}},
		{name: "s18 symlink to the manifest", reason: "entry type",
			entries: [][]byte{benign, patchedHeader("package/l.json", 0, '2', field(157, "package.json"))}},
		{name: "sparse name hides a pax path", reason: "pax key",
			entries: [][]byte{benign, paxEntry(paxRecords(
				"path", manifestEntry,
				"GNU.sparse.major", "0", "GNU.sparse.minor", "1",
				"GNU.sparse.numblocks", "1", "GNU.sparse.map", "0,"+size,
				"GNU.sparse.realsize", size, "GNU.sparse.name", "package/harmless.js",
			)), smuggled}},
		{name: "long name then pax path", reason: "entry type",
			entries: [][]byte{benign, rawEntry("././@LongLink", "package/harmless.js\x00", 'L'), paxEntry(paxRecords("path", manifestEntry)), smuggled}},
		{name: "pax path then long name", reason: "entry type",
			entries: [][]byte{benign, paxEntry(paxRecords("path", manifestEntry)), rawEntry("././@LongLink", "package/harmless.js\x00", 'L'), smuggled}},
		{name: "case variant of the manifest", reason: "install over",
			entries: [][]byte{benign, rawEntry("package/PACKAGE.JSON", installedVariant, '0')}},
		{name: "other top directory", reason: "install over",
			entries: [][]byte{benign, rawEntry("other/package.json", installedVariant, '0')}},
		{name: "global pax path", reason: "entry type",
			entries: [][]byte{rawEntry("pax_global_header", paxRecords("path", manifestEntry), 'g'), benign, smuggled}},
		{name: "old GNU sparse type", reason: "entry type",
			entries: [][]byte{benign, rawHeader("package/harmless.js", 0, 'S', ustarMagic)}},

		// Headers.
		{name: "gnu magic", reason: "magic",
			entries: [][]byte{benign, rawHeader("package/z.js", 0, '0', "ustar  \x00")}},
		{name: "no magic", reason: "magic",
			entries: [][]byte{benign, rawHeader("package/z.js", 0, '0', "")}},
		{name: "wrong checksum", reason: "checksum",
			entries: [][]byte{benign, func() []byte {
				b := rawHeader("package/z.js", 0, '0', ustarMagic)
				b[0] ^= 1
				return b
			}()}},
		{name: "checksum missing", reason: "checksum",
			entries: [][]byte{benign, func() []byte {
				b := rawHeader("package/z.js", 0, '0', ustarMagic)
				clear(b[148:156])
				return b
			}()}},
		{name: "base-256 size", reason: "numeric field",
			entries: [][]byte{benign, reg(field(124, "\x80\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))}},
		{name: "size with leading spaces", reason: "numeric field",
			entries: [][]byte{benign, reg(field(124, "         0 \x00"))}},
		{name: "size with a non-octal digit", reason: "numeric field",
			entries: [][]byte{benign, reg(field(124, "00000000008\x00"))}},
		{name: "size with digits after its terminator", reason: "numeric field",
			entries: [][]byte{benign, reg(field(124, "0000000 000\x00"))}},
		{name: "size all NUL", reason: "numeric field",
			entries: [][]byte{benign, reg(emptyField(124, 12))}},
		{name: "mode all NUL", reason: "numeric field",
			entries: [][]byte{benign, reg(emptyField(100, 8))}},
		{name: "mtime base-256", reason: "numeric field",
			entries: [][]byte{benign, reg(field(136, "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))}},
		{name: "uid with leading NULs", reason: "numeric field",
			entries: [][]byte{benign, reg(field(108, "\x00\x00\x00\x001\x00\x00\x00"))}},
		{name: "devmajor not a number", reason: "numeric field",
			entries: [][]byte{benign, reg(field(329, "abc\x00"))}},
		{name: "hardlink", reason: "entry type",
			entries: [][]byte{benign, rawHeader("package/l", 0, '1', ustarMagic)}},
		{name: "character device", reason: "entry type",
			entries: [][]byte{benign, rawHeader("package/l", 0, '3', ustarMagic)}},
		{name: "contiguous file", reason: "entry type",
			entries: [][]byte{benign, rawEntry("package/c", "x", '7')}},
		{name: "text after the name's terminator", reason: "name field",
			entries: [][]byte{benign, reg(field(50, "\x00x"))}},
		{name: "text after the prefix's terminator", reason: "name field",
			entries: [][]byte{benign, reg(field(345, "a\x00b"))}},
		{name: "times in the prefix field", reason: "name field",
			entries: [][]byte{benign, reg(field(476, "00000000000\x00"))}},
		{name: "bytes in the header padding", reason: "padding",
			entries: [][]byte{benign, reg(field(505, "x"))}},

		{name: "checksum running into the type flag", reason: "checksum",
			entries: [][]byte{benign, checksumOfEightDigits("package/z.js")}},

		// A pax header and the name it replaces.
		{name: "unnamed pax header", reason: "pax header has no name",
			entries: [][]byte{benign, rawEntry("", paxRecords("comment", "1"), 'x'), smuggled}},
		{name: "pax path replacing an empty name", reason: "replaces an empty name",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry("", "x", '0')}},
		{name: "pax path replacing the manifest's name", reason: "could be the manifest",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry(manifestEntry, installedVariant, '0')}},
		{name: "pax path replacing the manifest's name with itself", reason: "could be the manifest",
			entries: [][]byte{paxEntry(paxRecords("path", manifestEntry)), benign}},
		{name: "pax path replacing a case variant", reason: "could be the manifest",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry("package/PACKAGE.JSON", installedVariant, '0')}},
		{name: "pax path replacing another top directory", reason: "could be the manifest",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry("other/package.json", installedVariant, '0')}},
		{name: "pax path replacing a name split over the prefix", reason: "could be the manifest",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), prefixedEntry("package", "package.json", installedVariant)}},
		{name: "pax path replacing the archive root's manifest", reason: "could be the manifest",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry("package.json", installedVariant, '0')}},

		// A prefix is part of the name.
		{name: "manifest in prefix and name after a plain one", reason: "more than one",
			entries: [][]byte{benign, prefixedEntry("package", "package.json", installedVariant)}},
		{name: "plain manifest after one in prefix and name", reason: "more than one",
			entries: [][]byte{prefixedEntry("package", "package.json", installedVariant), benign}},
		{name: "alias split over the prefix", reason: "install over",
			entries: [][]byte{benign, prefixedEntry("other", "package.json", installedVariant)}},

		// Names.
		{name: "absolute name", reason: "name",
			entries: [][]byte{benign, rawHeader("/package/z.js", 0, '0', ustarMagic)}},
		{name: "dot segment", reason: "name",
			entries: [][]byte{benign, rawHeader("package/./z.js", 0, '0', ustarMagic)}},
		{name: "parent segment", reason: "name",
			entries: [][]byte{benign, rawHeader("package/../z.js", 0, '0', ustarMagic)}},
		{name: "empty segment", reason: "name",
			entries: [][]byte{benign, rawHeader("package//z.js", 0, '0', ustarMagic)}},
		{name: "backslash", reason: "name",
			entries: [][]byte{benign, rawHeader(`package\z.js`, 0, '0', ustarMagic)}},
		{name: "invalid UTF-8", reason: "name",
			entries: [][]byte{benign, rawHeader("package/\xff.js", 0, '0', ustarMagic)}},
		{name: "directory with a size", reason: "directory",
			entries: [][]byte{benign, rawEntry("package/d/", "x", '5')}},
		{name: "directory named as the manifest", reason: "not a regular file",
			entries: [][]byte{rawHeader(manifestEntry, 0, '5', ustarMagic)}},
		{name: "directory over the manifest", reason: "install over",
			entries: [][]byte{benign, rawHeader("other/package.json/", 0, '5', ustarMagic)}},
		{name: "traversal hidden in a pax path", reason: "name",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/../package.json")), smuggled}},
		{name: "empty pax path", reason: "name",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "")), smuggled}},

		// Pax headers.
		{name: "pax size differing from the header", reason: "pax size",
			entries: [][]byte{benign, paxEntry(paxRecords("size", "0")), smuggled}},
		{name: "pax size spelled differently from the header", reason: "pax size",
			entries: [][]byte{benign, paxEntry(paxRecords("size", "0"+size)), smuggled}},
		{name: "pax size on a directory", reason: "pax size",
			entries: [][]byte{benign, paxEntry(paxRecords("size", size)), rawHeader("package/d", 0, '5', ustarMagic), hidden[512:]}},
		{name: "pax size twice", reason: "pax key",
			entries: [][]byte{benign, paxEntry(paxRecords("size", size, "size", size)), smuggled}},
		{name: "pax unknown key", reason: "pax key",
			entries: [][]byte{benign, paxEntry(paxRecords("uname", "x")), smuggled}},
		{name: "pax duplicate key", reason: "pax key",
			entries: [][]byte{benign, paxEntry(paxRecords("path", "package/a.js", "path", "package/b.js")), smuggled}},
		{name: "pax over its bound", reason: "pax header",
			entries: [][]byte{benign, paxEntry(paxRecords("comment", strings.Repeat("x", 16<<10))), smuggled}},
		{name: "pax with no entry after it", reason: "pax header",
			entries: [][]byte{benign, paxEntry(paxRecords("comment", "1"))}},
		{name: "pax wrong length", reason: "pax record length",
			entries: [][]byte{benign, paxEntry("99 path=package/a.js\n"), smuggled}},
		{name: "pax length with a leading zero", reason: "pax record length",
			entries: [][]byte{benign, paxEntry("012 mtime=1\n"), smuggled}},
		{name: "pax record without a newline", reason: "pax record does not end",
			entries: [][]byte{benign, paxEntry("11 mtime=1x"), smuggled}},
		{name: "pax record with an inner newline", reason: "pax record has an inner newline",
			entries: [][]byte{benign, paxEntry(paxRecords("comment", "a\nb")), smuggled}},
		{name: "pax record without an equals sign", reason: "pax record has no key",
			entries: [][]byte{benign, paxEntry("11 nothing\n"), smuggled}},
		{name: "pax record with no length", reason: "pax record length",
			entries: [][]byte{benign, paxEntry("mtime=1\n"), smuggled}},
		{name: "pax with a NUL", reason: "pax record is not text",
			entries: [][]byte{benign, paxEntry(paxRecords("comment", "a\x00b")), smuggled}},
		{name: "pax with invalid UTF-8", reason: "pax record is not text",
			entries: [][]byte{benign, paxEntry(paxRecords("comment", "a\xffb")), smuggled}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(rawTarball(t, tc.entries...)))
			if err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want it to give the reason %q", err, tc.reason)
			}
		})
	}
}

func TestInspectManifest_RefusesBadEndsOfArchive(t *testing.T) {
	t.Parallel()
	benign := rawEntry(manifestEntry, readBySindri, '0')
	zero := make([]byte, 512)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	cases := []struct {
		name   string
		raw    []byte
		reason string
	}{
		{name: "no end marker", raw: benign, reason: "end-of-archive"},
		{name: "one zero block then the end", raw: cat(benign, zero), reason: "end-of-archive"},
		{name: "one zero block then an entry", raw: cat(benign, zero, benign), reason: "end-of-archive"},
		{name: "half a zero block", raw: cat(benign, zero[:256]), reason: "end-of-archive"},
		{name: "second zero block cut short", raw: cat(benign, zero, zero[:256]), reason: "end-of-archive"},
		{name: "partial header", raw: cat(benign, benign[:100]), reason: "end-of-archive"},
		{name: "entry after the end marker", raw: cat(benign, endOfArchive(), rawEntry(manifestEntry, installedVariant, '0')), reason: "after the end"},
		{name: "one byte after the end marker", raw: cat(benign, endOfArchive(), []byte{1}), reason: "after the end"},
		{name: "byte far past the end marker", raw: cat(benign, endOfArchive(), make([]byte, 64<<10), []byte{1}), reason: "after the end"},
		{name: "truncated body", raw: cat(rawHeader(manifestEntry, 2048, '0', ustarMagic), zero), reason: "end-of-archive"},
		{name: "truncated skipped body", raw: cat(benign, rawHeader("package/z.js", 2048, '0', ustarMagic), zero), reason: "end-of-archive"},
		{name: "truncated pax body", raw: cat(rawHeader("PaxHeader/x", 2048, 'x', ustarMagic), zero), reason: "end-of-archive"},
		{name: "pax then the end marker", raw: cat(benign, paxEntry(paxRecords("comment", "1")), endOfArchive()), reason: "pax header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(gzipped(t, tc.raw)))
			if err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want it to give the reason %q", err, tc.reason)
			}
		})
	}
}

func TestInspectManifest_RefusesAnythingButOneGzipMember(t *testing.T) {
	t.Parallel()
	good := rawTarball(t, rawEntry(manifestEntry, readBySindri, '0'))
	cases := []struct {
		name string
		data []byte
	}{
		{name: "second member", data: append(bytes.Clone(good), good...)},
		{name: "empty second member", data: append(bytes.Clone(good), gzipped(t, nil)...)},
		{name: "one byte after the member", data: append(bytes.Clone(good), 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectManifest(bytes.NewReader(tc.data))
			if err == nil {
				t.Fatalf("inspectManifest = %s, want an error", got)
			}
			if !strings.Contains(err.Error(), "after its gzip stream") {
				t.Errorf("err = %v, want it to name the trailing bytes", err)
			}
		})
	}
}

func TestInspectManifest_RefusalsWrapOneSentinelAndEchoNothing(t *testing.T) {
	t.Parallel()
	const secret = "npm_supersecret"
	benign := rawEntry(manifestEntry, readBySindri, '0')
	good := rawTarball(t, benign)
	badChecksum := bytes.Clone(good)
	badChecksum[len(badChecksum)-8] ^= 0xff

	cases := []struct {
		name string
		data []byte
	}{
		{name: "bad name", data: rawTarball(t, benign, rawHeader("package/../"+secret, 0, '0', ustarMagic), benign)},
		{name: "bad pax key", data: rawTarball(t, benign, paxEntry(paxRecords(secret, "1")), benign)},
		{name: "bad pax path", data: rawTarball(t, benign, paxEntry(paxRecords("path", "../"+secret)), benign)},
		{name: "linkname", data: rawTarball(t, benign, patchedHeader("package/z.js", 0, '0', func(b []byte) { copy(b[157:], secret) }), benign)},
		{name: "manifest with a syntax error", data: rawTarball(t, rawEntry(manifestEntry, secret, '0'))},
		{name: "manifest with a syntax error mid-document", data: rawTarball(t, rawEntry(manifestEntry, `{"a":`+secret+`}`, '0'))},
		{name: "manifest of the wrong type", data: rawTarball(t, rawEntry(manifestEntry, `"`+secret+`"`, '0'))},
		{name: "manifest null", data: rawTarball(t, rawEntry(manifestEntry, "null", '0'))},
		{name: "not gzip", data: []byte(secret)},
		{name: "corrupt deflate", data: append(bytes.Clone(good[:10]), bytes.Repeat([]byte{0xff}, 64)...)},
		{name: "bad gzip checksum", data: badChecksum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := inspectManifest(bytes.NewReader(tc.data))
			if !errors.Is(err, errTarballRefused) {
				t.Fatalf("err = %v, want errTarballRefused", err)
			}
			// A json.SyntaxError quotes the offending byte, hence the second check.
			if text := err.Error(); strings.Contains(text, "supersecret") || strings.Contains(text, "invalid character") {
				t.Errorf("error %q echoes the tarball", text)
			}
		})
	}
}

// Writing a hand-built header in each tool's exact field format pins what they
// put in fields the parser checks but nothing reads.
func TestInspectManifest_AcceptsHeadersRealToolsWrite(t *testing.T) {
	t.Parallel()
	benign := rawEntry(manifestEntry, readBySindri, '0')
	overUstar := "package/" + strings.Repeat("d/", 150) + "f.js"

	// tar-stream 3.2: numbers end in a space then a NUL, or a space alone for
	// size and mtime, and a long path goes on a regular entry named PaxHeader.
	tarStream := func(name string, size int, typeflag byte) []byte {
		b := make([]byte, 512)
		copy(b, name)
		copy(b[100:], "000644 \x00")
		copy(b[108:], "000000 \x00")
		copy(b[116:], "000000 \x00")
		copy(b[124:], fmt.Sprintf("%011o ", size))
		copy(b[136:], "15272107331 ")
		b[156] = typeflag
		copy(b[257:], ustarMagic)
		copy(b[329:], "000000 \x00")
		copy(b[337:], "000000 \x00")
		copy(b[148:], "        ")
		sum := 0
		for _, c := range b {
			sum += int(c)
		}
		copy(b[148:], fmt.Sprintf("%06o \x00", sum))
		return b
	}
	tarStreamPAX := paxRecords("path", overUstar)

	// npm pack leaves uid and gid entirely NUL.
	npmUnowned := func(b []byte) { clear(b[108:124]) }
	withPrefix := rawHeader(strings.Repeat("f", 40)+".js", 1, '0', ustarMagic)
	copy(withPrefix[345:], "package/"+strings.Repeat("d", 90))
	sealHeader(withPrefix)

	cases := []struct {
		name    string
		entries [][]byte
	}{
		{name: "tar-stream form", entries: [][]byte{
			append(tarStream(manifestEntry, len(readBySindri), '0'), padded(readBySindri)...),
			append(tarStream("PaxHeader", len(tarStreamPAX), 'x'), padded(tarStreamPAX)...),
			append(tarStream("PaxHeader", 1, '0'), padded("x")...),
			tarStream("package/dir", 0, '5'),
		}},
		{name: "npm form with no owner", entries: [][]byte{
			append(patchedHeader(manifestEntry, len(readBySindri), '0', npmUnowned), padded(readBySindri)...),
		}},
		{name: "ustar prefix", entries: [][]byte{benign, withPrefix, make([]byte, 512)}},
		{name: "pax path over a truncated name", entries: [][]byte{
			benign, paxEntry(paxRecords("path", overUstar)), rawEntry(overUstar[:100], "x", '0'),
		}},
		{name: "pax path over a name cut inside a character", entries: [][]byte{
			benign, paxEntry(paxRecords("path", "package/é.js")), rawEntry("package/\xc3", "x", '0'),
		}},
		{name: "pax path naming the manifest over another name", entries: [][]byte{
			paxEntry(paxRecords("path", manifestEntry)), rawEntry("package/long-name-cut-short", readBySindri, '0'),
		}},
		{name: "manifest in prefix and name", entries: [][]byte{
			prefixedEntry("package", "package.json", readBySindri),
		}},
		{name: "pax path replacing a name that ends in a slash", entries: [][]byte{
			benign, paxEntry(paxRecords("path", "package/a.js")), rawEntry("package/cut-short/", "x", '0'),
		}},
		{name: "pax with every allowed key", entries: [][]byte{
			benign,
			paxEntry(paxRecords("path", "package/a.js", "mtime", "1.5", "atime", "1", "ctime", "1", "comment", "a=b")),
			rawEntry("package/a.js", "x", '0'),
		}},
		{name: "pax size repeating the header's, as npm pack writes it", entries: [][]byte{
			benign,
			paxEntry(paxRecords("path", overUstar, "mtime", "499162500", "size", "1")),
			rawEntry(overUstar[:100], "x", '0'),
		}},
		{name: "pax with no path keeps the header's name", entries: [][]byte{
			paxEntry(paxRecords("mtime", "1")), benign,
		}},
		{name: "NUL typeflag is a regular file", entries: [][]byte{
			append(rawHeader(manifestEntry, len(readBySindri), 0, ustarMagic), padded(readBySindri)...),
		}},
		{name: "directories, with and without a slash", entries: [][]byte{
			rawHeader("package", 0, '5', ustarMagic), rawHeader("package/lib/", 0, '5', ustarMagic), benign,
		}},
		{name: "plain second file", entries: [][]byte{benign, rawEntry("package/also-harmless.js", installedVariant, '0')}},
		{name: "extra zero blocks", entries: [][]byte{benign, make([]byte, 8192)}},
		{name: "zero bytes short of a block", entries: [][]byte{benign, make([]byte, 1024+7)}},
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

// Real output of npm 10.9.9's npm pack, committed so over-refusal is pinned
// without the test needing npm.
func TestInspectManifest_AcceptsRealNpmPack(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		name string
	}{
		{file: "npm-pack-pax-path.tgz", name: "sindri-fixture-long-path"},
		{file: "npm-pack-ustar-prefix.tgz", name: "sindri-fixture-ustar-prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			got, err := inspectManifest(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("inspectManifest: %v", err)
			}
			var m struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			if m.Name != tc.name {
				t.Errorf("name = %q, want %q", m.Name, tc.name)
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
		reason  string
	}{
		{name: "uppercase base name alone", reason: "install over", entries: []tarEntry{alias("package/PACKAGE.JSON")}},
		{name: "uppercase base name after the manifest", reason: "install over", entries: []tarEntry{manifest, alias("package/PACKAGE.JSON")}},
		{name: "uppercase base name before the manifest", reason: "install over", entries: []tarEntry{alias("package/PACKAGE.JSON"), manifest}},
		{name: "mixed case base name", reason: "install over", entries: []tarEntry{manifest, alias("package/Package.json")}},
		{name: "uppercase directory alone", reason: "install over", entries: []tarEntry{alias("PACKAGE/package.json")}},
		{name: "uppercase directory after the manifest", reason: "install over", entries: []tarEntry{manifest, alias("PACKAGE/package.json")}},
		{name: "uppercase archive root", reason: "install over", entries: []tarEntry{manifest, alias("PACKAGE.JSON")}},
		{name: "backslash alone", reason: "entry name", entries: []tarEntry{alias(`package\package.json`)}},
		{name: "backslash after the manifest", reason: "entry name", entries: []tarEntry{manifest, alias(`package\package.json`)}},
		{name: "backslash and uppercase", reason: "entry name", entries: []tarEntry{manifest, alias(`package\PACKAGE.JSON`)}},
		{name: "backslash traversal", reason: "entry name", entries: []tarEntry{manifest, alias(`package\x\..\package.json`)}},
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

	t.Run("a nested variant is not an alias", func(t *testing.T) {
		t.Parallel()
		tarball := buildTarball(t, manifest, alias("package/nested/PACKAGE.JSON"))
		if _, err := inspectManifest(bytes.NewReader(tarball)); err != nil {
			t.Fatalf("inspectManifest: %v", err)
		}
	})
}
