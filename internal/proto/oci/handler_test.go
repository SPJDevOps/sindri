package oci

import (
	"net/http"
	"testing"
)

func TestParseContentRangeStart(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   int64
		ok     bool
	}{
		{name: "OCIForm", header: "0-21", want: 0, ok: true},
		{name: "OCIFormMid", header: "22-43", want: 22, ok: true},
		{name: "HTTPForm", header: "bytes 0-21/*", want: 0, ok: true},
		{name: "HTTPFormSized", header: "bytes 10-20/100", want: 10, ok: true},
		{name: "Empty", header: "", ok: false},
		{name: "Garbage", header: "nope", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseContentRangeStart(tc.header)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("start = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSplitRepoPath(t *testing.T) {
	name, rest, ok := splitRepoPath("demo/app/blobs/uploads/")
	if !ok || name != "demo/app" || rest != "blobs/uploads/" {
		t.Fatalf("got name=%q rest=%q ok=%v", name, rest, ok)
	}
	name, rest, ok = splitRepoPath("a/b/c/manifests/latest")
	if !ok || name != "a/b/c" || rest != "manifests/latest" {
		t.Fatalf("got name=%q rest=%q ok=%v", name, rest, ok)
	}
}

func TestAcceptHeader_JoinsEveryLine(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"NoHeaderIsEmpty", nil, ""},
		{"SingleLineIsUnchanged", []string{"application/vnd.oci.image.index.v1+json"}, "application/vnd.oci.image.index.v1+json"},
		{"CommaJoinedLineIsUnchanged", []string{"a/x, b/y"}, "a/x, b/y"},
		{"SeveralLinesAreAllKept", []string{"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.index.v1+json"},
			"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, l := range tc.lines {
				h.Add("Accept", l)
			}
			if got := acceptHeader(h); got != tc.want {
				t.Fatalf("acceptHeader = %q, want %q", got, tc.want)
			}
		})
	}
}
