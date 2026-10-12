package web_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"testing"

	"github.com/spj/sindri/web"
)

// Every vendored file must hash to what VENDOR.md records, so a swapped or
// tampered script cannot slip in as an unreviewed binary diff.
func TestVendoredAssets_MatchRecordedChecksums(t *testing.T) {
	vendor, err := web.Vendor()
	if err != nil {
		t.Fatalf("reading VENDOR.md: %v", err)
	}
	rows := regexp.MustCompile("\\| `static/([^`]+)` \\|.*\\| `([0-9a-f]{64})` \\|").FindAllStringSubmatch(string(vendor), -1)
	if len(rows) < 4 {
		t.Fatalf("VENDOR.md records %d files, want the script and three fonts", len(rows))
	}
	for _, row := range rows {
		path, want := row[1], row[2]
		b, err := fs.ReadFile(web.Static(), path)
		if err != nil {
			t.Errorf("%s is recorded but not embedded: %v", path, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s hashes to %s, VENDOR.md records %s", path, got, want)
		}
	}
}

func TestVersion_IsStableAndShort(t *testing.T) {
	if len(web.Version) != 12 {
		t.Fatalf("Version = %q", web.Version)
	}
}
