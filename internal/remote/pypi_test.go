package remote

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestAbsolutizePyPIFileURLs_ResolvesAgainstThePage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		page string
		link string
		want string
	}{
		{"parent-relative link against a nested page", "http://h/alice/prod/+simple/demo/", "../../+f/ab/d.whl", "http://h/alice/prod/+f/ab/d.whl"},
		{"absolute link is left unchanged", "http://h/alice/prod/+simple/demo/", "https://files.example.net/d.whl", "https://files.example.net/d.whl"},
		{"root-relative link keeps the page origin", "http://h/alice/prod/+simple/demo/", "/x/y/d.whl", "http://h/x/y/d.whl"},
		{"page after a redirect is the base", "http://h/idx/v2/demo/", "../../+f/d.whl", "http://h/idx/+f/d.whl"},
		{"fragment survives", "http://h/s/demo/", "../f/d.whl#sha256=ab", "http://h/s/f/d.whl#sha256=ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page, err := url.Parse(tc.page)
			if err != nil {
				t.Fatal(err)
			}
			in, _ := json.Marshal(map[string]any{
				"name": "demo", "extra": 7,
				"files": []map[string]any{{"filename": "d.whl", "url": tc.link, "hashes": map[string]string{"sha256": "ab"}}},
			})
			out, err := absolutizePyPIFileURLs(in, page)
			if err != nil {
				t.Fatalf("absolutize: %v", err)
			}
			idx, err := parsePyPIIndex(out)
			if err != nil || len(idx.Files) != 1 {
				t.Fatalf("parse: %v, %d files", err, len(idx.Files))
			}
			if idx.Files[0].URL != tc.want {
				t.Fatalf("url = %q, want %q", idx.Files[0].URL, tc.want)
			}
			if idx.Files[0].Hashes["sha256"] != "ab" {
				t.Fatalf("hashes lost: %v", idx.Files[0].Hashes)
			}
		})
	}
}
