package npm

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // G505: npm's dist.shasum field, never used for identity
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spj/sindri/internal/model"
)

const (
	// maxUnpackedBytes bounds the decompressed stream, so a gzip bomb costs a
	// fixed amount of CPU.
	maxUnpackedBytes = 1 << 30
	// maxManifestBytes bounds package.json.
	maxManifestBytes = 1 << 20
	// maxPAXBytes bounds a pax header, which only has to hold one long path.
	maxPAXBytes = 16 << 10

	manifestEntry = "package/package.json"

	blockSize = 512

	typeRegular byte = '0'
	typeDir     byte = '5'
	typePAX     byte = 'x'
)

var (
	errUnpackedTooLarge = errors.New("tarball unpacks to too much data")
	// errTarballRefused marks a tarball outside the grammar scanManifest accepts.
	errTarballRefused = errors.New("tarball refused")
)

// tarballInfo is what Sindri computes about a tarball's bytes. These values, and
// never the client's, are what a reader sees (ADR 24).
type tarballInfo struct {
	Size      int64
	SHA256    model.Digest
	Integrity string
	Shasum    string
}

// hashTarball computes every hash npm needs in one pass.
func hashTarball(r io.Reader) (tarballInfo, error) {
	sum256 := sha256.New()
	sum512 := sha512.New()
	sum1 := sha1.New() //nolint:gosec // G401: npm's dist.shasum field, never used for identity

	size, err := io.Copy(io.MultiWriter(sum256, sum512, sum1), r)
	if err != nil {
		return tarballInfo{}, fmt.Errorf("hashing tarball: %w", err)
	}
	return tarballInfo{
		Size:      size,
		SHA256:    model.NewDigest(model.AlgorithmSHA256, sum256.Sum(nil)),
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sum512.Sum(nil)),
		Shasum:    hex.EncodeToString(sum1.Sum(nil)),
	}, nil
}

// inspectManifest returns the tarball's package.json with the fields only
// Sindri may set removed. It is what the packument later serves, so the document
// a reader sees is the one that gets installed.
func inspectManifest(r io.Reader) (json.RawMessage, error) {
	return scanManifest(r, maxUnpackedBytes)
}

// scanManifest checks the whole stream against tarball grammar, under a bound
// on its decompressed size.
//
// It is its own parser rather than archive/tar because an installer parses this
// tarball too, and every point where two parsers disagree can hide a second
// package.json from one of them. Go and node-tar disagree about many header
// shapes (ADR 24), so this accepts only what npm pack and tar-stream write and
// refuses the rest:
//
//   - one gzip member, and nothing after it
//   - ustar headers with an unsigned checksum and plain octal numbers
//   - regular files and directories, each optionally preceded by one pax header
//     holding a few known keys, and no links
//   - names with one spelling: no dot, dot-dot, or empty segment, and no backslash
//   - two zero blocks at the end, then only zero bytes
func scanManifest(r io.Reader, maxUnpacked int64) (json.RawMessage, error) {
	// A bufio.Reader is an io.ByteReader, so gzip reads no further than its
	// member and what follows can be checked.
	compressed := bufio.NewReader(r)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("reading tarball gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)

	// One byte past the bound, to tell a stream at the bound from one over it.
	unpacked := &io.LimitedReader{R: gz, N: maxUnpacked + 1}
	manifest, err := readEntries(unpacked)
	if unpacked.N <= 0 {
		return nil, errUnpackedTooLarge
	}
	if err != nil {
		return nil, err
	}

	switch _, err := compressed.ReadByte(); {
	case err == nil:
		return nil, refuse("tarball has bytes after its gzip stream")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("reading tarball gzip: %w", err)
	}
	if manifest == nil {
		return nil, refuse("tarball has no package/package.json")
	}
	return sanitizeManifest(manifest)
}

// refuse is the error for a tarball outside the grammar. Reasons are fixed text:
// nothing from the tarball goes into them.
func refuse(reason string) error {
	return fmt.Errorf("%w: %s", errTarballRefused, reason)
}

// readEntries walks the decompressed stream to its end and returns the manifest's
// bytes, or nil if no entry was the manifest.
func readEntries(in io.Reader) ([]byte, error) {
	var (
		manifest []byte
		block    = make([]byte, blockSize)
		pax      *paxHeader
	)
	for {
		if err := readFull(in, block); err != nil {
			return nil, err
		}
		if isZero(block) {
			if pax != nil {
				return nil, refuse("pax header with no entry after it")
			}
			if err := readEndOfArchive(in, block); err != nil {
				return nil, err
			}
			return manifest, nil
		}

		hdr, err := parseHeader(block)
		if err != nil {
			return nil, err
		}
		if hdr.typeflag == typePAX {
			if pax != nil {
				return nil, refuse("two pax headers in a row")
			}
			if pax, err = readPAXHeader(in, hdr.size); err != nil {
				return nil, err
			}
			continue
		}

		name := hdr.name
		if pax != nil {
			if pax.hasPath {
				name = pax.path
			}
			if pax.hasSize && pax.size != strconv.FormatInt(hdr.size, 10) {
				return nil, refuse("pax size differs from the header's size")
			}
		}
		pax = nil

		isDir := hdr.typeflag == typeDir
		if err := checkEntryName(name, isDir); err != nil {
			return nil, err
		}
		if isDir && hdr.size != 0 {
			return nil, refuse("directory entry has a size")
		}
		switch {
		case name == manifestEntry:
			if manifest != nil {
				return nil, refuse("tarball has more than one package/package.json")
			}
			if isDir {
				return nil, refuse("tarball's package/package.json is not a regular file")
			}
			if hdr.size > maxManifestBytes {
				return nil, refuse("tarball's package/package.json is too large")
			}
			manifest = make([]byte, hdr.size)
			if err := readFull(in, manifest); err != nil {
				return nil, err
			}
			if err := skip(in, paddedSize(hdr.size)-hdr.size); err != nil {
				return nil, err
			}
		case installsAsManifest(name):
			return nil, refuse("tarball has an entry that would install over package.json")
		default:
			if err := skip(in, paddedSize(hdr.size)); err != nil {
				return nil, err
			}
		}
	}
}

// readEndOfArchive is called after one zero block. It leaves in at its end, so
// that gzip has verified its checksum.
func readEndOfArchive(in io.Reader, block []byte) error {
	if err := readFull(in, block); err != nil {
		return err
	}
	if !isZero(block) {
		return refuse("tarball has one zero block where its end-of-archive marker needs two")
	}
	for {
		n, err := in.Read(block)
		if !isZero(block[:n]) {
			return refuse("tarball has data after the end of its archive")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tarball: %w", err)
		}
	}
}

// readFull fills b. Running out of stream anywhere inside the archive means the
// end marker was never reached.
func readFull(in io.Reader, b []byte) error {
	_, err := io.ReadFull(in, b)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return refuse("tarball ends without an end-of-archive marker")
	}
	if err != nil {
		return fmt.Errorf("reading tarball: %w", err)
	}
	return nil
}

func skip(in io.Reader, n int64) error {
	_, err := io.CopyN(io.Discard, in, n)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return refuse("tarball ends without an end-of-archive marker")
	}
	if err != nil {
		return fmt.Errorf("reading tarball: %w", err)
	}
	return nil
}

func paddedSize(size int64) int64 {
	return (size + blockSize - 1) / blockSize * blockSize
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// tarHeader is what a header block says, once it has passed parseHeader.
type tarHeader struct {
	typeflag byte
	name     string // prefix and name joined; a pax path may replace it
	size     int64
}

// parseHeader checks one 512-byte ustar header. Fields nothing here reads are
// checked too, because a parser that ignores a field and one that honours it are
// how a tarball gets two meanings. uname and gname are the exception: they have
// no effect on what gets installed.
func parseHeader(b []byte) (tarHeader, error) {
	if string(b[257:263]) != "ustar\x00" || string(b[263:265]) != "00" {
		return tarHeader{}, refuse("header magic is not ustar")
	}
	if !checksumMatches(b) {
		return tarHeader{}, refuse("header checksum does not match")
	}

	var typeflag byte
	switch b[156] {
	case 0, typeRegular:
		typeflag = typeRegular
	case typeDir, typePAX:
		typeflag = b[156]
	default:
		return tarHeader{}, refuse("entry type is not allowed")
	}

	size, ok := parseOctal(b[124:136])
	if !ok {
		return tarHeader{}, refuse("numeric field is malformed")
	}
	for _, field := range [][]byte{b[100:108], b[136:148], b[329:337], b[337:345]} { // mode, mtime, devmajor, devminor
		if _, ok := parseOctal(field); !ok {
			return tarHeader{}, refuse("numeric field is malformed")
		}
	}
	for _, field := range [][]byte{b[108:116], b[116:124]} { // uid, gid: npm pack leaves these all NUL
		if _, ok := parseOctal(field); !ok && !isZero(field) {
			return tarHeader{}, refuse("numeric field is malformed")
		}
	}

	if !isZero(b[157:257]) {
		return tarHeader{}, refuse("entry has a linkname")
	}
	if !isZero(b[500:512]) {
		return tarHeader{}, refuse("header padding is not empty")
	}

	name, ok := terminatedString(b[0:100])
	if !ok {
		return tarHeader{}, refuse("name field has text after its terminator")
	}
	prefix, ok := terminatedString(b[345:500])
	if !ok {
		return tarHeader{}, refuse("name field has text after its terminator")
	}
	if prefix != "" {
		name = prefix + "/" + name
	}
	return tarHeader{typeflag: typeflag, name: name, size: size}, nil
}

// checksumMatches sums the block as unsigned bytes, with the checksum field read
// as spaces. Other parsers also accept a signed sum; that is one of the ways to
// make a block a header to one parser and file content to another.
func checksumMatches(b []byte) bool {
	want, ok := parseOctal(b[148:156])
	if !ok {
		return false
	}
	var sum int64
	for i, c := range b {
		if i >= 148 && i < 156 {
			c = ' '
		}
		sum += int64(c)
	}
	return sum == want
}

// parseOctal reads octal digits with optional trailing spaces or NULs. A field is
// at most 12 bytes, so the value cannot overflow.
func parseOctal(field []byte) (int64, bool) {
	var value int64
	i := 0
	for ; i < len(field) && field[i] >= '0' && field[i] <= '7'; i++ {
		value = value*8 + int64(field[i]-'0')
	}
	if i == 0 {
		return 0, false
	}
	for _, c := range field[i:] {
		if c != ' ' && c != 0 {
			return 0, false
		}
	}
	return value, true
}

// terminatedString reads a NUL-terminated field that must be empty after the NUL.
func terminatedString(field []byte) (string, bool) {
	end := bytes.IndexByte(field, 0)
	if end < 0 {
		return string(field), true
	}
	return string(field[:end]), isZero(field[end:])
}

// paxHeader is what a pax extended header says about the next entry.
type paxHeader struct {
	path    string
	hasPath bool
	size    string
	hasSize bool
}

func readPAXHeader(in io.Reader, size int64) (*paxHeader, error) {
	if size > maxPAXBytes {
		return nil, refuse("pax header is too large")
	}
	body := make([]byte, paddedSize(size))
	if err := readFull(in, body); err != nil {
		return nil, err
	}
	return parsePAXRecords(body[:size])
}

// parsePAXRecords reads "<length> <key>=<value>\n" records. Only keys that cannot
// change where an entry's bytes are or end up are allowed, along with path, and
// size, which npm pack writes beside every long path and which the caller
// requires to repeat the header's own size.
func parsePAXRecords(body []byte) (*paxHeader, error) {
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return nil, refuse("pax record is malformed")
	}
	pax := &paxHeader{}
	seen := make(map[string]bool)
	for len(body) > 0 {
		lengthText, _, ok := bytes.Cut(body, []byte(" "))
		if !ok {
			return nil, refuse("pax record is malformed")
		}
		length, err := strconv.Atoi(string(lengthText))
		if err != nil || strconv.Itoa(length) != string(lengthText) ||
			length <= len(lengthText)+1 || length > len(body) || body[length-1] != '\n' {
			return nil, refuse("pax record is malformed")
		}
		record := body[len(lengthText)+1 : length-1]
		body = body[length:]

		key, value, ok := bytes.Cut(record, []byte("="))
		if !ok || bytes.IndexByte(record, '\n') >= 0 {
			return nil, refuse("pax record is malformed")
		}
		switch string(key) {
		case "path":
			pax.path, pax.hasPath = string(value), true
		case "size":
			pax.size, pax.hasSize = string(value), true
		case "mtime", "atime", "ctime", "comment":
		default:
			return nil, refuse("pax key is not allowed")
		}
		if seen[string(key)] {
			return nil, refuse("pax key appears twice")
		}
		seen[string(key)] = true
	}
	return pax, nil
}

// checkEntryName allows each path exactly one spelling. A directory may end in a
// slash, which is how tools write them; a regular file may not, because old tar
// readers take that to mean a directory with no body.
func checkEntryName(name string, isDir bool) error {
	if strings.HasSuffix(name, "/") {
		if !isDir {
			return refuse("regular entry has a trailing slash")
		}
		name = strings.TrimSuffix(name, "/")
	}
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") {
		return refuse("entry name is malformed")
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return refuse("entry name is malformed")
		}
	}
	return nil
}

// installsAsManifest reports whether an entry that is not spelled exactly
// package/package.json could still land on it. Installers strip the first path
// component whatever it is, and the last entry wins, so a tarball holding
// package/package.json and other/package.json would show readers one manifest
// and install another: the manifest confusion ADR 24 exists to prevent. Case is
// folded because the filesystem installing the package may fold it.
func installsAsManifest(name string) bool {
	clean := strings.TrimSuffix(name, "/")
	return strings.EqualFold(path.Base(clean), "package.json") && strings.Count(clean, "/") <= 1
}

// sanitizeManifest drops the fields that describe the registry's copy rather
// than the package. Keys are matched without regard to case because
// encoding/json, which reads these documents later, matches struct fields that way.
func sanitizeManifest(raw []byte) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parsing package/package.json: %w", err)
	}
	if fields == nil {
		return nil, errors.New("package/package.json is not a JSON object")
	}
	for key := range fields {
		if strings.HasPrefix(key, "_") ||
			strings.EqualFold(key, "dist") || strings.EqualFold(key, "readme") || strings.EqualFold(key, "deprecated") {
			delete(fields, key)
		}
	}
	sanitized, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding package/package.json: %w", err)
	}
	return sanitized, nil
}
