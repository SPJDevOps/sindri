package registry

import (
	"errors"
	"testing"
)

func TestValidateRepoName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "internal"},
		{name: "nested", in: "team/internal"},
		{name: "punctuation", in: "team.a_b-c/internal"},
		{name: "illegal character", in: "team internal", wantErr: true},
		{name: "leading slash", in: "/internal", wantErr: true},
		{name: "trailing slash", in: "internal/", wantErr: true},
		{name: "empty segment", in: "team//internal", wantErr: true},
		// A repository name that contains an adapter's path marker makes the
		// route ambiguous: /pypi/team/packages/simple/foo/ would name either
		// the simple index of "team/packages" or a download from "team".
		{name: "reserved pypi simple", in: "team/simple", wantErr: true},
		{name: "reserved pypi packages", in: "team/packages/x", wantErr: true},
		{name: "reserved pypi upload", in: "upload", wantErr: true},
		{name: "reserved oci blobs", in: "team/blobs", wantErr: true},
		{name: "reserved oci manifests", in: "manifests/team", wantErr: true},
		{name: "reserved oci tags", in: "team/tags/x", wantErr: true},
		{name: "reserved regardless of case", in: "team/Packages", wantErr: true},
		{name: "reserved word as substring is fine", in: "team/packages-internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRepoName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRepoName(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validateRepoName(%q) error does not wrap ErrInvalidInput: %v", tc.in, err)
			}
		})
	}
}

func TestNormalizePackageName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "pep503 fold", in: "Foo.Bar_Baz", want: "foo-bar-baz"},
		{name: "runs collapse", in: "foo___bar", want: "foo-bar"},
		{name: "separators only", in: "___", want: ""},
		{name: "slashes are dropped", in: "a/b", want: "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizePackageName(tc.in); got != tc.want {
				t.Fatalf("normalizePackageName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
