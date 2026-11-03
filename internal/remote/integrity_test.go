package remote

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: testing npm's dist.shasum, which the code under test accepts only as a fallback
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/spj/sindri/internal/model"
)

func TestIntegrityCheck_PicksTheStrongestAndChecksTheBytes(t *testing.T) {
	t.Parallel()
	body := []byte("tarball bytes")
	s512 := sha512.Sum512(body)
	s384 := sha512.Sum384(body)
	s256 := sha256.Sum256(body)
	s1 := sha1.Sum(body) //nolint:gosec // G401: see the import
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	good512 := "sha512-" + b64(s512[:])
	good384 := "sha384-" + b64(s384[:])
	good256 := "sha256-" + b64(s256[:])
	goodSRI1 := "sha1-" + b64(s1[:])
	goodShasum := hex.EncodeToString(s1[:])

	wrong := []byte("other bytes")
	w512 := sha512.Sum512(wrong)
	w1 := sha1.Sum(wrong) //nolint:gosec // G401: see the import
	bad512 := "sha512-" + b64(w512[:])
	badShasum := hex.EncodeToString(w1[:])

	cases := []struct {
		name      string
		integrity string
		shasum    string
		wantAlg   string
		wantMatch bool
	}{
		{name: "sha512", integrity: good512, wantAlg: "sha512", wantMatch: true},
		{name: "sha384", integrity: good384, wantAlg: "sha384", wantMatch: true},
		{name: "sha256", integrity: good256, wantAlg: "sha256", wantMatch: true},
		{name: "sha1 shasum alone", shasum: goodShasum, wantAlg: "sha1", wantMatch: true},
		{name: "sha1 in SRI", integrity: goodSRI1, wantAlg: "sha1", wantMatch: true},
		{name: "strongest of several wins", integrity: good256 + " " + good512 + " " + good384, wantAlg: "sha512", wantMatch: true},
		{name: "SRI options are ignored", integrity: good512 + "?foo=bar", wantAlg: "sha512", wantMatch: true},
		{name: "sha512 outranks a shasum", integrity: good512, shasum: badShasum, wantAlg: "sha512", wantMatch: true},
		{name: "wrong sha512 fails even with a good shasum", integrity: bad512, shasum: goodShasum, wantAlg: "sha512"},
		{name: "wrong shasum fails", shasum: badShasum, wantAlg: "sha1"},
		{name: "malformed integrity falls back to shasum", integrity: "sha512-!!!", shasum: goodShasum, wantAlg: "sha1", wantMatch: true},
		{name: "truncated digest is not usable", integrity: "sha512-" + b64(s512[:10]), shasum: goodShasum, wantAlg: "sha1", wantMatch: true},
		{name: "unknown algorithm is not usable", integrity: "md5-" + b64(s1[:16]), shasum: goodShasum, wantAlg: "sha1", wantMatch: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := newIntegrityCheck(tc.integrity, tc.shasum)
			if !ok {
				t.Fatal("no usable hash")
			}
			if check.algorithm != tc.wantAlg {
				t.Fatalf("algorithm = %s, want %s", check.algorithm, tc.wantAlg)
			}
			_, _ = check.hash.Write(body)
			if got := check.matches(); got != tc.wantMatch {
				t.Fatalf("matches = %v, want %v", got, tc.wantMatch)
			}
		})
	}
}

// The check runs in finish, so a mismatch fails the final read and nothing else.
func TestCachingBody_IntegrityFailsTheFinalRead(t *testing.T) {
	t.Parallel()
	body := []byte("tarball bytes")
	sum := sha512.Sum512(body)
	good := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	other := sha512.Sum512([]byte("other"))
	bad := "sha512-" + base64.StdEncoding.EncodeToString(other[:])

	cases := []struct {
		name      string
		integrity string
		wantErr   bool
	}{
		{name: "Match", integrity: good},
		{name: "Mismatch", integrity: bad, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check, ok := newIntegrityCheck(tc.integrity, "")
			if !ok {
				t.Fatal("no usable hash")
			}
			b := &cachingBody{
				ctx:         t.Context(),
				log:         slog.New(slog.DiscardHandler),
				body:        io.NopCloser(bytes.NewReader(body)),
				cancel:      func() {},
				idle:        time.NewTimer(time.Hour),
				idleTimeout: time.Hour,
				digester:    model.NewDigester(model.AlgorithmSHA256),
				size:        -1,
				integrity:   check,
			}
			defer func() { _ = b.Close() }()
			got, err := io.ReadAll(b)
			if !bytes.Equal(got, body) {
				t.Fatalf("read %q; the bytes stream either way", got)
			}
			if tc.wantErr && !errors.Is(err, ErrUpstreamUnavailable) {
				t.Fatalf("ReadAll = %v, want ErrUpstreamUnavailable", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ReadAll = %v", err)
			}
		})
	}
}

func TestIntegrityCheck_NoUsableHashIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		integrity string
		shasum    string
	}{
		{name: "nothing"},
		{name: "blank", integrity: "   ", shasum: " "},
		{name: "not SRI", integrity: "sha512"},
		{name: "bad base64", integrity: "sha512-###"},
		{name: "unknown algorithm", integrity: "md5-AAAA"},
		{name: "shasum not hex", shasum: "zz"},
		{name: "shasum wrong length", shasum: "abcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if check, ok := newIntegrityCheck(tc.integrity, tc.shasum); ok {
				t.Fatalf("accepted %q %q as %s", tc.integrity, tc.shasum, check.algorithm)
			}
		})
	}
}
