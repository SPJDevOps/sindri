package npm

import "testing"

func TestParseRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want route
	}{
		{name: "unscoped packument", path: "team/internal/foo.bar",
			want: route{kind: routePackument, repo: "team/internal", pkg: "foo.bar"}},
		{name: "scoped packument lowercase escape", path: "team/internal/@acme%2fui",
			want: route{kind: routePackument, repo: "team/internal", pkg: "@acme/ui"}},
		{name: "scoped packument uppercase escape", path: "team/internal/@acme%2Fui",
			want: route{kind: routePackument, repo: "team/internal", pkg: "@acme/ui"}},
		{name: "scoped packument escaped at", path: "team/internal/%40acme%2fui",
			want: route{kind: routePackument, repo: "team/internal", pkg: "@acme/ui"}},
		{name: "scoped packument plain slash", path: "team/internal/@acme/ui",
			want: route{kind: routePackument, repo: "team/internal", pkg: "@acme/ui"}},
		{name: "scoped packument escaped at plain slash", path: "r/%40acme/ui",
			want: route{kind: routePackument, repo: "r", pkg: "@acme/ui"}},
		{name: "package named like an endpoint", path: "r/whoami",
			want: route{kind: routePackument, repo: "r", pkg: "whoami"}},
		{name: "unscoped tarball", path: "r/foo.bar/-/foo.bar-1.0.0.tgz",
			want: route{kind: routeTarball, repo: "r", pkg: "foo.bar", file: "foo.bar-1.0.0.tgz"}},
		{name: "scoped tarball", path: "team/internal/@acme/ui/-/ui-1.0.0.tgz",
			want: route{kind: routeTarball, repo: "team/internal", pkg: "@acme/ui", file: "ui-1.0.0.tgz"}},
		{name: "scoped tarball escaped", path: "r/@acme%2fui/-/ui-1.0.0.tgz",
			want: route{kind: routeTarball, repo: "r", pkg: "@acme/ui", file: "ui-1.0.0.tgz"}},
		{name: "dist-tags", path: "r/-/package/@acme%2fui/dist-tags",
			want: route{kind: routeDistTags, repo: "r", pkg: "@acme/ui"}},
		{name: "dist-tag", path: "r/-/package/foo/dist-tags/beta",
			want: route{kind: routeDistTags, repo: "r", pkg: "foo", tag: "beta"}},
		{name: "whoami", path: "r/-/whoami",
			want: route{kind: routeWhoami, repo: "r"}},
		{name: "whoami nested repository", path: "team/internal/-/whoami",
			want: route{kind: routeWhoami, repo: "team/internal"}},
		{name: "web login", path: "r/-/v1/login",
			want: route{kind: routeWebLogin, repo: "r"}},
		{name: "user login", path: "r/-/user/org.couchdb.user:capture",
			want: route{kind: routeUserLogin, repo: "r", user: "capture"}},
		{name: "user login escaped username", path: "r/-/user/org.couchdb.user:a%40b",
			want: route{kind: routeUserLogin, repo: "r", user: "a@b"}},
	}
	covered := make(map[routeKind]bool)
	for _, tc := range cases {
		covered[tc.want.kind] = true
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRoute(tc.path)
			if !ok {
				t.Fatalf("parseRoute(%q) refused", tc.path)
			}
			if got != tc.want {
				t.Fatalf("parseRoute(%q) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
	for kind := routeKind(0); kind < routeKindCount; kind++ {
		if !covered[kind] {
			t.Errorf("no case covers route kind %d", kind)
		}
	}
}

func TestParseRoute_Refused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{name: "no repository", path: "foo"},
		{name: "empty", path: ""},
		{name: "endpoint without repository", path: "-/whoami"},
		{name: "unsupported endpoint", path: "r/-/npm/v1/security/advisories/bulk"},
		{name: "two slashes in a package", path: "r/@acme%2fa%2fb"},
		{name: "dist-tags without the suffix", path: "r/-/package/foo"},
		{name: "tarball with a subdirectory", path: "r/foo/-/a/b.tgz"},
		{name: "escaped repository", path: "r%2fs/foo"},
		{name: "escaped repository before a scope", path: "r%2fs/@acme/ui"},
		{name: "escaped repository before a tarball", path: "r%2fs/foo/-/foo-1.0.0.tgz"},
		{name: "escaped repository before an endpoint", path: "r%2fs/-/whoami"},
		{name: "escaped dash cannot split a path", path: "r/%2d/whoami"},
		{name: "scoped name without repository", path: "@acme/ui"},
		{name: "scoped tarball without repository", path: "@acme/ui/-/ui-1.0.0.tgz"},
		{name: "tarball without repository", path: "foo/-/foo-1.0.0.tgz"},
		{name: "tarball without package", path: "r/-/foo-1.0.0.tgz"},
		{name: "scope alone", path: "r/@acme"},
		{name: "escaped scope with an empty name", path: "r/@acme%2f"},
		{name: "slash without a scope", path: "r/a%2fb"},
		{name: "scope with an extra segment", path: "r/@acme%2fui/extra"},
		{name: "invalid escape", path: "r/foo%zz"},
		{name: "leading slash", path: "/r/foo"},
		{name: "trailing slash", path: "r/foo/"},
		{name: "empty repository segment", path: "team//foo"},
		{name: "tarball name escapes its segment", path: "r/foo/-/..%2f..%2fetc.tgz"},
		{name: "tarball name with backslash", path: "r/foo/-/..%5cetc.tgz"},
		{name: "trailing path after whoami", path: "r/-/whoami/x"},
		{name: "login without a user", path: "r/-/user/org.couchdb.user:"},
		{name: "login for another document", path: "r/-/user/capture"},
		{name: "dist-tags with an extra segment", path: "r/-/package/foo/dist-tags/a/b"},
		{name: "dist-tag with an escaped slash", path: "r/-/package/foo/dist-tags/a%2fb"},
		{name: "dist-tags for a malformed package", path: "r/-/package/a%2fb/dist-tags"},
		{name: "second dash after the leftmost", path: "r/-/-/whoami"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := parseRoute(tc.path); ok {
				t.Fatalf("parseRoute(%q) = %+v, want refusal", tc.path, got)
			}
		})
	}
}
