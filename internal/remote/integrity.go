package remote

import (
	"crypto/sha1" //nolint:gosec // G505: npm's dist.shasum, the only hash old packages have; never used for identity, the blob key stays sha256
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"strings"
)

// integrityCheck is the hash an npm packument promises for a tarball, computed
// beside the sha256 that keys the blob.
type integrityCheck struct {
	algorithm string
	hash      hash.Hash
	want      []byte
}

// matches reports whether the bytes written to hash are the promised ones.
func (c *integrityCheck) matches() bool {
	return subtle.ConstantTimeCompare(c.hash.Sum(nil), c.want) == 1
}

// npmHashes lists the algorithms an npm SRI string may use, strongest first.
// sha1 is last because it is all a package published before 2017 has.
var npmHashes = []struct {
	name string
	make func() hash.Hash
}{
	{"sha512", sha512.New},
	{"sha384", sha512.New384},
	{"sha256", sha256.New},
	{"sha1", sha1.New}, //nolint:gosec // G401: as at the import
}

// newIntegrityCheck picks the strongest usable hash from dist.integrity, an SRI
// string of space-separated "algorithm-base64" tokens, and otherwise from
// dist.shasum, which is hex sha1. A token that does not decode to its hash's
// length is not usable. false means nothing can verify the bytes, and a tarball
// that cannot be verified is not cached or served (ADR 24).
func newIntegrityCheck(integrity, shasum string) (*integrityCheck, bool) {
	tokens := strings.Fields(integrity)
	for _, candidate := range npmHashes {
		h := candidate.make()
		for _, token := range tokens {
			algorithm, value, ok := strings.Cut(token, "-")
			if !ok || algorithm != candidate.name {
				continue
			}
			value, _, _ = strings.Cut(value, "?")
			want, err := base64.StdEncoding.DecodeString(value)
			if err == nil && len(want) == h.Size() {
				return &integrityCheck{algorithm: candidate.name, hash: h, want: want}, true
			}
		}
	}
	want, err := hex.DecodeString(strings.TrimSpace(shasum))
	if err != nil || len(want) != sha1.Size {
		return nil, false
	}
	return &integrityCheck{algorithm: "sha1", hash: sha1.New(), want: want}, true //nolint:gosec // G401: as at the import
}
