// Package model holds the domain types shared across Sindri's packages. It sits
// at the bottom of the dependency graph and imports nothing from the rest of the
// project, so every other package may depend on it.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// ErrInvalidDigest is returned when a string is not a well formed digest.
var ErrInvalidDigest = errors.New("invalid digest")

// Algorithm names a content hash function. Sindri stores everything under
// sha256; the type exists so that a second algorithm is an addition rather than
// a change of shape.
type Algorithm string

// AlgorithmSHA256 is the only algorithm Sindri writes.
const AlgorithmSHA256 Algorithm = "sha256"

// hexLen is the encoded length of a digest for each supported algorithm.
var hexLen = map[Algorithm]int{
	AlgorithmSHA256: sha256.Size * 2,
}

// Digest identifies content by its hash. It is a struct rather than a named
// string so that the only way to obtain one is to parse or compute it: internal
// code cannot be handed a malformed digest, and the zero value is invalid rather
// than being an empty but usable key.
type Digest struct {
	algo Algorithm
	hex  string
}

// ParseDigest validates and parses the canonical "<algorithm>:<hex>" form. The
// hex is required to be lowercase, because the digest is also a storage key and
// two spellings of one digest would be two objects.
func ParseDigest(s string) (Digest, error) {
	algo, encoded, ok := strings.Cut(s, ":")
	if !ok {
		return Digest{}, fmt.Errorf("parsing digest %q: %w: want <algorithm>:<hex>", s, ErrInvalidDigest)
	}

	want, known := hexLen[Algorithm(algo)]
	if !known {
		return Digest{}, fmt.Errorf("parsing digest %q: %w: unsupported algorithm %q", s, ErrInvalidDigest, algo)
	}
	if len(encoded) != want {
		return Digest{}, fmt.Errorf("parsing digest %q: %w: %s needs %d hex characters, got %d",
			s, ErrInvalidDigest, algo, want, len(encoded))
	}
	if strings.ToLower(encoded) != encoded {
		return Digest{}, fmt.Errorf("parsing digest %q: %w: hex must be lowercase", s, ErrInvalidDigest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return Digest{}, fmt.Errorf("parsing digest %q: %w: hex is not valid", s, ErrInvalidDigest)
	}

	return Digest{algo: Algorithm(algo), hex: encoded}, nil
}

// NewDigest builds a Digest from a raw hash sum. It panics on a sum of the wrong
// length, which is an impossible state: the caller passed a sum from a different
// hash function than it named.
func NewDigest(algo Algorithm, sum []byte) Digest {
	want, known := hexLen[algo]
	if !known {
		panic(fmt.Sprintf("model.NewDigest: unsupported algorithm %q", algo))
	}
	if len(sum)*2 != want {
		panic(fmt.Sprintf("model.NewDigest: %s needs a %d byte sum, got %d", algo, want/2, len(sum)))
	}
	return Digest{algo: algo, hex: hex.EncodeToString(sum)}
}

// Valid reports whether d carries a parsed digest rather than being the zero value.
func (d Digest) Valid() bool { return d.algo != "" && d.hex != "" }

// Algorithm returns the hash function that produced d.
func (d Digest) Algorithm() Algorithm { return d.algo }

// Hex returns the encoded hash without the algorithm prefix.
func (d Digest) Hex() string { return d.hex }

// String renders the canonical "<algorithm>:<hex>" form. The zero value renders
// as an empty string so that a mistakenly logged zero digest is obvious.
func (d Digest) String() string {
	if !d.Valid() {
		return ""
	}
	return string(d.algo) + ":" + d.hex
}

// MarshalText implements encoding.TextMarshaler.
func (d Digest) MarshalText() ([]byte, error) {
	if !d.Valid() {
		return nil, fmt.Errorf("marshalling digest: %w: zero value", ErrInvalidDigest)
	}
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler, so that a digest arriving
// in JSON or YAML is validated at the edge like any other.
func (d *Digest) UnmarshalText(b []byte) error {
	parsed, err := ParseDigest(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Digester computes a Digest over a stream. Callers write the bytes they
// actually received through it, never the bytes a client claimed to send.
type Digester struct {
	algo Algorithm
	h    hash.Hash
	n    int64
}

// NewDigester returns a Digester for algo. It panics on an unsupported
// algorithm, which is a programming error rather than input.
func NewDigester(algo Algorithm) *Digester {
	if algo != AlgorithmSHA256 {
		panic(fmt.Sprintf("model.NewDigester: unsupported algorithm %q", algo))
	}
	return &Digester{algo: algo, h: sha256.New()}
}

// Write implements io.Writer and never returns an error.
func (d *Digester) Write(p []byte) (int, error) {
	n, err := d.h.Write(p)
	d.n += int64(n)
	if err != nil {
		return n, fmt.Errorf("hashing %d bytes: %w", len(p), err)
	}
	return n, nil
}

// Digest returns the digest of everything written so far.
func (d *Digester) Digest() Digest { return NewDigest(d.algo, d.h.Sum(nil)) }

// Size returns the number of bytes written so far.
func (d *Digester) Size() int64 { return d.n }

// Tee returns a reader that feeds everything read from r into the digester.
func (d *Digester) Tee(r io.Reader) io.Reader { return io.TeeReader(r, d) }
