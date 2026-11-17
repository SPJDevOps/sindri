package web_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"strings"
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
	recorded := map[string]bool{}
	for _, row := range rows {
		recorded[row[1]] = true
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

func TestOwnScript_IsRecordedAndHasNoUnsafeConstructs(t *testing.T) {
	vendor, err := web.Vendor()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vendor), "## Own files") || !strings.Contains(string(vendor), "`static/sindri.js`") {
		t.Error("VENDOR.md must record static/sindri.js under Own files")
	}
	b, err := fs.ReadFile(web.Static(), "sindri.js")
	if err != nil {
		t.Fatal(err)
	}
	// The script must work under a Content-Security-Policy of 'self' and must
	// not turn page or server text into markup.
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`\beval\s*\(`),
		regexp.MustCompile(`new\s+Function\b`),
		regexp.MustCompile(`\.(?:inner|outer)HTML\s*(?:\+?=|\()`),
		regexp.MustCompile(`insertAdjacentHTML`),
		regexp.MustCompile(`document\.write`),
		regexp.MustCompile(`setAttribute\(\s*["']style["']`),
		regexp.MustCompile(`set(?:Timeout|Interval)\(\s*["'` + "`" + `]`),
	} {
		if loc := bad.Find(b); loc != nil {
			t.Errorf("sindri.js contains %q, which breaks the CSP or invites XSS", loc)
		}
	}
}

func TestVersion_IsStableAndShort(t *testing.T) {
	if len(web.Version) != 12 {
		t.Fatalf("Version = %q", web.Version)
	}
}
