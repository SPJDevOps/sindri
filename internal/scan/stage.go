package scan

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
)

const (
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	annotationReferenceType = "vnd.docker.reference.type"
	attestationManifest     = "attestation-manifest"

	// maxManifestBytes bounds a manifest or index read into memory.
	maxManifestBytes = 8 << 20

	// maxArchiveEntries caps the entries of an archive: files, directories
	// and skipped entries alike.
	maxArchiveEntries = 100000
	// entryCharge is charged against MaxUnpackedBytes for every entry, so
	// an archive of empty files cannot exhaust inodes for free.
	entryCharge = 512
	// maxPlatforms caps the distinct scannable children of one index.
	maxPlatforms = 64
)

var (
	// errTooManyPlatforms means an index lists more distinct scannable
	// children than maxPlatforms. The job attaches nothing.
	errTooManyPlatforms = errors.New("index has too many platforms")
	// errNoPlatforms means an index has no scannable child.
	errNoPlatforms = errors.New("index has no scannable platforms")
)

// target is one thing Trivy is pointed at.
type target struct {
	// dir is an OCI image layout (image) or an unpacked tree (package).
	dir string
	// coverage is "image" or "package".
	coverage string
	// platform is set for an index child, as os/arch[/variant].
	platform string
}

// stage writes the subject's bytes under root and returns what to scan.
func (s *Scanner) stage(ctx context.Context, root string, kind model.SubjectKind, digest model.Digest) ([]target, error) {
	switch kind {
	case model.SubjectKindOCIManifest:
		dir := filepath.Join(root, "image")
		if err := s.stageManifest(ctx, dir, digest); err != nil {
			return nil, err
		}
		return []target{{dir: dir, coverage: "image"}}, nil
	case model.SubjectKindOCIIndex:
		return s.stageIndex(ctx, root, digest)
	case model.SubjectKindPyPIDist, model.SubjectKindNPMTarball:
		dir := filepath.Join(root, "package")
		if err := s.stageArchive(ctx, dir, digest); err != nil {
			return nil, err
		}
		return []target{{dir: dir, coverage: "package"}}, nil
	case model.SubjectKindFile:
		return nil, fmt.Errorf("staging %s: %w", digest, ErrUnsupportedKind)
	}
	return nil, fmt.Errorf("staging %s of kind %q: %w", digest, kind, ErrUnsupportedKind)
}

func (s *Scanner) readJSON(ctx context.Context, digest model.Digest, v any) ([]byte, error) {
	rc, err := s.store.Get(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", digest, err)
	}
	if len(raw) > maxManifestBytes {
		return nil, fmt.Errorf("reading %s: document larger than %d bytes", digest, maxManifestBytes)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", digest, err)
	}
	return raw, nil
}

// stageManifest writes an OCI image layout for the image manifest at digest.
func (s *Scanner) stageManifest(ctx context.Context, dir string, digest model.Digest) error {
	var manifest ocispec.Manifest
	raw, err := s.readJSON(ctx, digest, &manifest)
	if err != nil {
		return err
	}
	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = ocispec.MediaTypeImageManifest
	}
	if mediaType != ocispec.MediaTypeImageManifest && mediaType != mediaTypeDockerManifest {
		return fmt.Errorf("manifest %s has media type %q: %w", digest, mediaType, ErrUnsupportedKind)
	}

	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return fmt.Errorf("creating image layout: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageLayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}

	index := ocispec.Index{
		Manifests: []ocispec.Descriptor{{MediaType: mediaType, Digest: godigest.Digest(digest.String()), Size: int64(len(raw))}},
	}
	index.SchemaVersion = 2
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("encoding index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageIndexFile), indexJSON, 0o600); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	if err := writeBlob(blobDir, digest, bytes.NewReader(raw), int64(len(raw))); err != nil {
		return err
	}
	descs := append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...)
	for _, d := range descs {
		if d.Size < 0 {
			return fmt.Errorf("manifest %s: descriptor %s has negative size %d", digest, d.Digest, d.Size)
		}
	}
	for _, d := range descs {
		dd, err := model.ParseDigest(d.Digest.String())
		if err != nil {
			return fmt.Errorf("manifest %s: descriptor digest: %w", digest, err)
		}
		rc, err := s.store.Get(ctx, dd)
		if err != nil {
			return fmt.Errorf("reading blob %s of manifest %s: %w", dd, digest, err)
		}
		err = writeBlob(blobDir, dd, rc, d.Size)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeBlob streams r to blobs/sha256/<hex> and checks the size against the
// descriptor. The bytes were verified when they entered the CAS.
func writeBlob(blobDir string, digest model.Digest, r io.Reader, want int64) error {
	if digest.Algorithm() != model.AlgorithmSHA256 {
		return fmt.Errorf("blob %s: only sha256 is supported in an image layout", digest)
	}
	// #nosec G304 -- blobDir is this process's staging directory and the name is a validated digest hex.
	f, err := os.OpenFile(filepath.Join(blobDir, digest.Hex()), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating blob %s: %w", digest, err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing blob %s: %w", digest, err)
	}
	if n != want {
		return fmt.Errorf("blob %s is %d bytes, descriptor says %d", digest, n, want)
	}
	return nil
}

// stageIndex stages every image child of an index, one layout per platform.
// Attestation manifests, nested indexes and unknown children are skipped.
func (s *Scanner) stageIndex(ctx context.Context, root string, digest model.Digest) ([]target, error) {
	var index ocispec.Index
	if _, err := s.readJSON(ctx, digest, &index); err != nil {
		return nil, err
	}
	type child struct {
		digest   model.Digest
		platform string
	}
	var children []child
	seen := make(map[string]bool)
	for _, c := range index.Manifests {
		if c.Annotations[annotationReferenceType] == attestationManifest {
			s.logger.InfoContext(ctx, "skipping attestation manifest in index", "index", digest.String(), "child", c.Digest.String())
			continue
		}
		if c.MediaType != ocispec.MediaTypeImageManifest && c.MediaType != mediaTypeDockerManifest {
			s.logger.InfoContext(ctx, "skipping index child that is not an image manifest",
				"index", digest.String(), "child", c.Digest.String(), "media_type", c.MediaType)
			continue
		}
		cd, err := model.ParseDigest(c.Digest.String())
		if err != nil {
			return nil, fmt.Errorf("index %s: child digest: %w", digest, err)
		}
		// A digest listed twice is staged once, under the first platform.
		if seen[cd.String()] {
			continue
		}
		seen[cd.String()] = true
		children = append(children, child{cd, platformString(c.Platform)})
	}
	// Count before staging anything: an over-large index costs no disk.
	if len(children) == 0 {
		return nil, fmt.Errorf("index %s: %w", digest, errNoPlatforms)
	}
	if len(children) > maxPlatforms {
		return nil, fmt.Errorf("index %s: %d distinct children, more than %d: %w", digest, len(children), maxPlatforms, errTooManyPlatforms)
	}
	var targets []target
	for i, c := range children {
		dir := filepath.Join(root, "image-"+strconv.Itoa(i))
		if err := s.stageManifest(ctx, dir, c.digest); err != nil {
			return nil, fmt.Errorf("staging child %s of index %s: %w", c.digest, digest, err)
		}
		targets = append(targets, target{dir: dir, coverage: "image", platform: c.platform})
	}
	return targets, nil
}

func platformString(p *ocispec.Platform) string {
	if p == nil || p.OS == "" || p.Architecture == "" {
		return ""
	}
	out := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		out += "/" + p.Variant
	}
	return out
}

// stageArchive unpacks the zip or gzipped tar at digest into dir. The kind is
// decided by content, not by file name.
func (s *Scanner) stageArchive(ctx context.Context, dir string, digest model.Digest) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	rc, err := s.store.Get(ctx, digest)
	if err != nil {
		return fmt.Errorf("reading %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()

	magic := make([]byte, 4)
	n, err := io.ReadFull(rc, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading %s: %w", digest, err)
	}
	magic = magic[:n]
	body := io.MultiReader(bytes.NewReader(magic), rc)

	budget := &unpackBudget{remaining: s.opts.MaxUnpackedBytes, entriesLeft: s.maxEntries}
	switch {
	case bytes.HasPrefix(magic, []byte("PK\x03\x04")):
		return unpackZip(ctx, dir, body, budget)
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return unpackTarGz(ctx, dir, body, budget)
	}
	return fmt.Errorf("archive %s is neither zip nor gzip: %w", digest, ErrUnsupportedKind)
}

// unpackBudget counts bytes actually written and entries seen.
type unpackBudget struct {
	remaining   int64
	entriesLeft int
}

// entry charges one archive entry, skipped or not, and stops early when the
// job is cancelled.
func (b *unpackBudget) entry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unpacking archive: %w", err)
	}
	b.entriesLeft--
	b.remaining -= entryCharge
	if b.entriesLeft < 0 || b.remaining < 0 {
		return ErrArchiveTooLarge
	}
	return nil
}

// copyFile writes r to path within the budget, with mode 0o600.
func (b *unpackBudget) copyFile(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	// #nosec G304 -- safeJoin has refused absolute and escaping names before path reaches here.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, b.remaining+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("unpacking file: %w", err)
	}
	if n > b.remaining {
		return ErrArchiveTooLarge
	}
	b.remaining -= n
	return nil
}

// safeJoin returns dir/name, or ErrUnsafePath if name is absolute or escapes.
func safeJoin(dir, name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", fmt.Errorf("entry %q: %w", name, ErrUnsafePath)
	}
	return filepath.Join(dir, filepath.FromSlash(name)), nil
}

func unpackTarGz(ctx context.Context, dir string, r io.Reader, budget *unpackBudget) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		if err := budget.entry(ctx); err != nil {
			return err
		}
		// Links, devices and the like are not needed to scan and are never
		// created, so nothing unpacked can point outside dir.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		path, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("creating directory: %w", err)
			}
			continue
		}
		if err := budget.copyFile(path, tr); err != nil {
			return fmt.Errorf("unpacking %q: %w", hdr.Name, err)
		}
	}
}

// unpackZip needs random access, so the archive is spooled to a file beside
// the destination first. The spool is bounded by the stored blob, and the
// unpacked bytes are bounded by the budget.
func unpackZip(ctx context.Context, dir string, r io.Reader, budget *unpackBudget) error {
	spool, err := os.CreateTemp(filepath.Dir(dir), "spool-*.zip")
	if err != nil {
		return fmt.Errorf("spooling archive: %w", err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	size, err := io.Copy(spool, r)
	if err != nil {
		return fmt.Errorf("spooling archive: %w", err)
	}
	zr, err := zip.NewReader(spool, size)
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	for _, zf := range zr.File {
		if err := budget.entry(ctx); err != nil {
			return err
		}
		mode := zf.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			continue
		}
		path, err := safeJoin(dir, zf.Name)
		if err != nil {
			return err
		}
		if mode.IsDir() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("creating directory: %w", err)
			}
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("opening %q: %w", zf.Name, err)
		}
		err = budget.copyFile(path, rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("unpacking %q: %w", zf.Name, err)
		}
	}
	return nil
}
