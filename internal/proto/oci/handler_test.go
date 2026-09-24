package oci

import "testing"

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
