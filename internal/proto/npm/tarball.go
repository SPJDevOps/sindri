package npm

import (
	"archive/tar"
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
	"strings"

	"github.com/spj/sindri/internal/model"
)

const (
	// maxUnpackedBytes bounds the decompressed stream, so a gzip bomb costs a
	// fixed amount of CPU.
	maxUnpackedBytes = 1 << 30
	// maxManifestBytes bounds package.json.
	maxManifestBytes = 1 << 20

	manifestEntry = "package/package.json"
)

var errUnpackedTooLarge = errors.New("tarball unpacks to too much data")

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

// scanManifest reads every entry rather than stopping at the manifest, because
// a duplicate after it is exactly what it has to catch.
func scanManifest(r io.Reader, maxUnpacked int64) (json.RawMessage, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("reading tarball gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	// One byte past the bound, to tell a stream at the bound from one over it.
	// The limit alone reads as a clean end of input, and at an entry boundary it
	// would end the scan quietly with later entries unseen.
	unpacked := &io.LimitedReader{R: gz, N: maxUnpacked + 1}
	overBound := func(err error) error {
		if unpacked.N <= 0 {
			return errUnpackedTooLarge
		}
		return err
	}

	var manifest []byte
	found := false
	tr := tar.NewReader(unpacked)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, overBound(fmt.Errorf("reading tarball entries: %w", err))
		}
		if err := checkEntryHeader(hdr); err != nil {
			return nil, err
		}
		switch {
		case hdr.Name == manifestEntry:
			if found {
				return nil, errors.New("tarball has more than one package/package.json")
			}
			found = true
			if hdr.Typeflag != tar.TypeReg {
				return nil, errors.New("tarball's package/package.json is not a regular file")
			}
			manifest, err = io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
			if err != nil {
				return nil, overBound(fmt.Errorf("reading package/package.json: %w", err))
			}
			if len(manifest) > maxManifestBytes {
				return nil, fmt.Errorf("tarball's package/package.json is larger than %d bytes", maxManifestBytes)
			}
		case installsAsManifest(hdr.Name):
			return nil, errors.New("tarball has an entry that would install over package.json")
		}
	}

	// Read to the end of the stream, which checks the gzip checksum and counts
	// whatever follows the tar's end-of-archive blocks towards the bound.
	if _, err := io.Copy(io.Discard, unpacked); err != nil {
		return nil, fmt.Errorf("reading tarball gzip: %w", err)
	}
	if unpacked.N <= 0 {
		return nil, errUnpackedTooLarge
	}
	if !found {
		return nil, errors.New("tarball has no package/package.json")
	}
	return sanitizeManifest(manifest)
}

// checkEntryHeader refuses an entry whose name depends on which tar parser reads
// it. Go lets a GNU long name or a sparse name override a pax path, and node-tar
// honours the path, so a name checked here would not be the name installed.
// After this, an entry has one name and checking hdr.Name checks all of them.
//
// Nothing real is lost. npm pack writes a pax path only as the entry's full name,
// which Go reports as hdr.Name, and it never writes sparse files.
func checkEntryHeader(hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return errors.New("tarball has a sparse entry")
	}
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return errors.New("tarball has a sparse entry")
		}
	}
	if paxPath, ok := hdr.PAXRecords["path"]; ok && paxPath != hdr.Name {
		return errors.New("tarball has an entry whose pax path differs from its name")
	}
	return nil
}

// installsAsManifest reports whether an entry that is not spelled exactly
// package/package.json could still land on it. Installers strip the first path
// component whatever it is, and the last entry wins, so a tarball holding
// package/package.json and other/package.json would show readers one manifest
// and install another: the manifest confusion ADR 24 exists to prevent. Case and
// backslashes are folded because the filesystem or platform installing the
// package may fold them.
func installsAsManifest(name string) bool {
	slashed := strings.ReplaceAll(name, `\`, "/")
	clean := strings.TrimPrefix(path.Clean("/"+slashed), "/")
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
