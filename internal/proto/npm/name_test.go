package npm

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "plain", in: "foo"},
		{name: "dot", in: "foo.bar"},
		{name: "underscore", in: "foo_bar"},
		{name: "scoped", in: "@acme/ui"},
		{name: "at the length limit", in: strings.Repeat("a", 214)},
		{name: "uppercase", in: "Foo", wantErr: true},
		{name: "uppercase scope", in: "@Acme/ui", wantErr: true},
		{name: "leading dot", in: ".foo", wantErr: true},
		{name: "leading underscore", in: "_foo", wantErr: true},
		{name: "scope alone", in: "@acme", wantErr: true},
		{name: "scope with empty name", in: "@acme/", wantErr: true},
		{name: "slash without a scope", in: "a/b", wantErr: true},
		{name: "reserved node_modules", in: "node_modules", wantErr: true},
		{name: "reserved favicon", in: "favicon.ico", wantErr: true},
		{name: "over the length limit", in: strings.Repeat("a", 215), wantErr: true},
		{name: "space", in: "foo bar", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "traversal in the scoped name", in: "@acme/..", wantErr: true},
		{name: "traversal in the scope", in: "@../foo", wantErr: true},
		{name: "two slashes", in: "@acme/a/b", wantErr: true},
		{name: "trailing newline", in: "foo\n", wantErr: true},
		{name: "percent", in: "foo%2fbar", wantErr: true},
		{name: "reserved route segment", in: "-", wantErr: true},
		{name: "reserved route segment under a scope", in: "@acme/-", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateName(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestValidateName_ErrorDoesNotEchoTheName(t *testing.T) {
	t.Parallel()
	const secret = "Hunter2 token"
	err := validateName(secret)
	if err == nil {
		t.Fatal("validateName accepted a malformed name")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q echoes the client value", err)
	}
}

func TestValidVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "release", in: "1.0.0", want: true},
		{name: "prerelease", in: "1.0.0-rc.1", want: true},
		{name: "at the length limit", in: "1.0.0-" + strings.Repeat("a", 250), want: true},
		{name: "short form", in: "1.0"},
		{name: "major only", in: "1"},
		{name: "v prefix", in: "v1.0.0"},
		{name: "build metadata", in: "1.0.0+build"},
		{name: "empty", in: ""},
		{name: "leading zero", in: "01.0.0"},
		{name: "path", in: "1.0.0/../x"},
		{name: "over the length limit", in: "1.0.0-" + strings.Repeat("a", 251)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validVersion(tc.in); got != tc.want {
				t.Fatalf("validVersion(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "latest", in: "latest", want: true},
		{name: "next", in: "next", want: true},
		{name: "dotted", in: "beta.1", want: true},
		{name: "at the length limit", in: strings.Repeat("a", 256), want: true},
		{name: "empty", in: ""},
		{name: "version", in: "1.0.0"},
		{name: "v version", in: "v1.0.0"},
		{name: "short version", in: "1"},
		{name: "short v version", in: "v1"},
		{name: "slash", in: "a/b"},
		{name: "space", in: "a b"},
		{name: "trailing newline", in: "beta\n"},
		{name: "current directory", in: "."},
		{name: "parent directory", in: ".."},
		{name: "over the length limit", in: strings.Repeat("a", 257)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validTag(tc.in); got != tc.want {
				t.Fatalf("validTag(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestTarballFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pkg     string
		version string
		want    string
	}{
		{name: "scoped", pkg: "@acme/ui", version: "1.0.0", want: "ui-1.0.0.tgz"},
		{name: "unscoped", pkg: "foo.bar", version: "1.0.0-rc.1", want: "foo.bar-1.0.0-rc.1.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tarballFilename(tc.pkg, tc.version); got != tc.want {
				t.Fatalf("tarballFilename(%q, %q) = %q, want %q", tc.pkg, tc.version, got, tc.want)
			}
		})
	}
}
