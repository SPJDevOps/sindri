//go:build integration

package main

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/testsupport"
)

// npmRepos declares the hosted npm repositories the tests publish into. The
// name with a slash is on purpose: it is what the adapter's path parsing and
// npm's per-registry credential lookup both have to get right.
const (
	npmRepo  = "team/internal"
	npmOther = "other"
	npmRepos = "repos:\n  " + npmRepo + ": { protocol: npm }\n  " + npmOther + ": { protocol: npm }\n"
)

// startNpmServe runs serve with publicUrl set to its own bound address, on the
// same host as the registry npm is pointed at, as a real deployment has it.
func startNpmServe(t *testing.T) *stack {
	t.Helper()
	addr := freeAddr(t)
	return startServeOn(t, addr, testsupport.DatabaseURL(t), "", "publicUrl: http://"+addr+"\n"+npmRepos, nil)
}

// npmClient runs the real npm binary against one repository with one stored
// token. Its configuration and caches live in a directory of the test's own, so
// the test never reads or changes the developer's npm setup. Audit stays at
// npm's default, because that is what a real install does.
type npmClient struct {
	t        *testing.T
	bin      string
	home     string
	registry string
}

// newNpm finds npm on PATH. A developer without it skips; CI installs node, so
// there a missing binary fails rather than skipping quietly. The token is
// stored the way npm login stores it, under the registry's own path.
func newNpm(t *testing.T, s *stack, repo, token string) *npmClient {
	t.Helper()
	bin, err := exec.LookPath("npm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("npm is not on PATH; CI must install node: %v", err)
		}
		t.Skip("npm is not on PATH")
	}
	n := &npmClient{t: t, bin: bin, home: t.TempDir(), registry: s.base + "/npm/" + repo + "/"}
	line := strings.TrimPrefix(n.registry, "http:") + ":_authToken=" + token + "\n"
	if err := os.WriteFile(filepath.Join(n.home, "npmrc"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return n
}

// run runs npm in dir with args, returning its combined output. Every run gets
// an empty cache, so an install really fetches its packument and tarballs.
func (n *npmClient) run(dir string, args ...string) (string, error) {
	n.t.Helper()
	cache, err := os.MkdirTemp(n.home, "cache-")
	if err != nil {
		n.t.Fatal(err)
	}
	cmd := exec.CommandContext(n.t.Context(), n.bin, args...) //nolint:gosec // G204: the test's own npm binary and arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"NPM_CONFIG_USERCONFIG="+filepath.Join(n.home, "npmrc"),
		"NPM_CONFIG_GLOBALCONFIG="+filepath.Join(n.home, "global-npmrc"),
		"NPM_CONFIG_CACHE="+cache,
		"NPM_CONFIG_REGISTRY="+n.registry,
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (n *npmClient) mustRun(dir string, args ...string) string {
	n.t.Helper()
	out, err := n.run(dir, args...)
	if err != nil {
		n.t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// mustFail runs npm expecting a non-zero exit whose output names the npm error
// code, so a failure for some other reason does not pass.
func (n *npmClient) mustFail(code, dir string, args ...string) {
	n.t.Helper()
	out, err := n.run(dir, args...)
	if err == nil {
		n.t.Fatalf("npm %s succeeded, want %s:\n%s", strings.Join(args, " "), code, out)
	}
	if !strings.Contains(out, code) {
		n.t.Fatalf("npm %s output lacks %s:\n%s", strings.Join(args, " "), code, out)
	}
}

// publish publishes name@version with real npm, passing any extra arguments on.
func (n *npmClient) publish(name, version string, extra ...string) {
	n.t.Helper()
	n.mustRun(writeNpmPackage(n.t, name, version), append([]string{"publish"}, extra...)...)
}

// install installs specs into a new, empty directory and returns the directory
// and npm's output.
func (n *npmClient) install(specs ...string) (string, string) {
	n.t.Helper()
	dir := n.t.TempDir()
	return dir, n.mustRun(dir, append([]string{"install"}, specs...)...)
}

// installedVersion installs name from spec and returns the version that landed.
func (n *npmClient) installedVersion(name, spec string) string {
	n.t.Helper()
	dir, _ := n.install(spec)
	raw, err := os.ReadFile(filepath.Join(dir, "node_modules", name, "package.json")) //nolint:gosec // G304: the file npm install was told to write
	if err != nil {
		n.t.Fatalf("npm install %s wrote no package: %v", spec, err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		n.t.Fatal(err)
	}
	return manifest.Version
}

// writeNpmPackage writes an ordinary package into a new directory of its own.
func writeNpmPackage(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	manifest, err := json.Marshal(map[string]string{"name": name, "version": version, "main": "index.js"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"package.json": string(manifest),
		"index.js":     fmt.Sprintf("module.exports = %q;\n", name+"@"+version),
	}
	for file, body := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// packIntegrity packs a package with real npm and returns the tarball's path and
// the sha512 the test computed from its bytes, so what is compared later is not
// npm's or Sindri's own word for the digest.
func packIntegrity(t *testing.T, n *npmClient, src string) (string, string) {
	t.Helper()
	dest := t.TempDir()
	n.mustRun(src, "pack", "--pack-destination", dest)
	matches, err := filepath.Glob(filepath.Join(dest, "*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("npm pack wrote %v (%v), want one tarball", matches, err)
	}
	tarball, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(tarball)
	return matches[0], "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// A package goes from a real publish to a real install in a directory that has
// never seen it, and the lockfile records a registry URL and the digest of the
// bytes that were packed.
func TestNpm_PublishThenInstallFromCleanDirectory(t *testing.T) {
	s := startNpmServe(t)

	tests := []struct {
		name string
		pkg  string
	}{
		{"Scoped", "@acme/demo"},
		{"DottedName", "foo.bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNpm(t, s, npmRepo, bootstrapToken)
			tarball, want := packIntegrity(t, n, writeNpmPackage(t, tt.pkg, "1.0.0"))
			n.mustRun(t.TempDir(), "publish", tarball)

			dir, _ := n.install(tt.pkg)
			if _, err := os.Stat(filepath.Join(dir, "node_modules", tt.pkg, "package.json")); err != nil {
				t.Fatalf("installed package: %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(dir, "package-lock.json")) //nolint:gosec // G304: the file npm install was told to write
			if err != nil {
				t.Fatal(err)
			}
			var lock struct {
				Packages map[string]struct {
					Resolved  string `json:"resolved"`
					Integrity string `json:"integrity"`
				} `json:"packages"`
			}
			if err := json.Unmarshal(raw, &lock); err != nil {
				t.Fatal(err)
			}
			entry, ok := lock.Packages["node_modules/"+tt.pkg]
			if !ok {
				t.Fatalf("package-lock.json has no entry for %s:\n%s", tt.pkg, raw)
			}
			if !strings.HasPrefix(entry.Resolved, "http://") || !strings.Contains(entry.Resolved, "/npm/"+npmRepo+"/") {
				t.Fatalf("resolved = %q, want an http URL under /npm/%s/", entry.Resolved, npmRepo)
			}
			if entry.Integrity != want {
				t.Fatalf("integrity = %q, want the sha512 of the packed bytes %q", entry.Integrity, want)
			}

			// Installing from a lockfile is when npm's default audit really runs,
			// and Sindri has no audit endpoint: that must not fail the install.
			n.mustRun(dir, "ci")
			if _, err := os.Stat(filepath.Join(dir, "node_modules", tt.pkg, "package.json")); err != nil {
				t.Fatalf("package after npm ci: %v", err)
			}
		})
	}
}

func TestNpm_RepublishFailsWithE409(t *testing.T) {
	s := startNpmServe(t)
	n := newNpm(t, s, npmRepo, bootstrapToken)
	n.publish("pkg", "1.0.0")
	n.mustFail("E409", writeNpmPackage(t, "pkg", "1.0.0"), "publish")
}

func TestNpm_InstallTagAndMovingLatest(t *testing.T) {
	s := startNpmServe(t)
	n := newNpm(t, s, npmRepo, bootstrapToken)
	n.publish("pkg", "1.0.0")
	n.publish("pkg", "2.0.0")
	n.publish("pkg", "3.0.0-rc.1", "--tag", "next")

	if got := n.installedVersion("pkg", "pkg@next"); got != "3.0.0-rc.1" {
		t.Fatalf("pkg@next installed %s, want 3.0.0-rc.1", got)
	}
	if got := n.installedVersion("pkg", "pkg"); got != "2.0.0" {
		t.Fatalf("plain install before the move got %s, want 2.0.0", got)
	}
	n.mustRun(t.TempDir(), "dist-tag", "add", "pkg@1.0.0", "latest")
	if got := n.installedVersion("pkg", "pkg"); got != "1.0.0" {
		t.Fatalf("plain install after moving latest got %s, want 1.0.0", got)
	}
}

func TestNpm_DeprecateWarnsOnInstall(t *testing.T) {
	s := startNpmServe(t)
	n := newNpm(t, s, npmRepo, bootstrapToken)
	n.publish("pkg", "1.0.0")
	const message = "use something newer"

	n.mustRun(t.TempDir(), "deprecate", "pkg@1.0.0", message)
	_, out := n.install("pkg")
	if !strings.Contains(out, "npm warn deprecated") || !strings.Contains(out, message) {
		t.Fatalf("install of a deprecated version did not warn:\n%s", out)
	}

	n.mustRun(t.TempDir(), "deprecate", "pkg@1.0.0", "")
	if _, out := n.install("pkg"); strings.Contains(out, "deprecated") {
		t.Fatalf("install still warns after clearing the deprecation:\n%s", out)
	}
}

func TestNpm_DistTagLsAddRm(t *testing.T) {
	s := startNpmServe(t)
	n := newNpm(t, s, npmRepo, bootstrapToken)
	n.publish("@acme/pkg", "1.0.0")
	n.publish("@acme/pkg", "2.0.0", "--tag", "next")
	dir := t.TempDir()

	if out := n.mustRun(dir, "dist-tag", "ls", "@acme/pkg"); !strings.Contains(out, "latest: 1.0.0") || !strings.Contains(out, "next: 2.0.0") {
		t.Fatalf("dist-tag ls after publishing:\n%s", out)
	}
	n.mustRun(dir, "dist-tag", "add", "@acme/pkg@2.0.0", "beta")
	if out := n.mustRun(dir, "dist-tag", "ls", "@acme/pkg"); !strings.Contains(out, "beta: 2.0.0") {
		t.Fatalf("dist-tag ls after add:\n%s", out)
	}
	n.mustRun(dir, "dist-tag", "rm", "@acme/pkg", "beta")
	if out := n.mustRun(dir, "dist-tag", "ls", "@acme/pkg"); strings.Contains(out, "beta") {
		t.Fatalf("dist-tag ls after rm:\n%s", out)
	}

	// A package always has a latest, so removing it is refused.
	if out, err := n.run(dir, "dist-tag", "rm", "@acme/pkg", "latest"); err == nil {
		t.Fatalf("dist-tag rm latest succeeded:\n%s", out)
	}
	if out := n.mustRun(dir, "dist-tag", "ls", "@acme/pkg"); !strings.Contains(out, "latest: 1.0.0") {
		t.Fatalf("dist-tag ls after refusing rm latest:\n%s", out)
	}
}

func TestNpm_ReadOnlyTokenCannotMoveATag(t *testing.T) {
	s := startNpmServe(t)
	admin := newNpm(t, s, npmRepo, bootstrapToken)
	admin.publish("pkg", "1.0.0")
	admin.publish("pkg", "2.0.0", "--tag", "next")

	reader := s.issueToken(`{"name":"ci-read","repository":"` + npmRepo + `","role":"read"}`)
	ro := newNpm(t, s, npmRepo, reader.Token)
	ro.mustFail("E403", t.TempDir(), "dist-tag", "add", "pkg@2.0.0", "latest")

	if out := admin.mustRun(t.TempDir(), "dist-tag", "ls", "pkg"); !strings.Contains(out, "latest: 1.0.0") {
		t.Fatalf("the refused move changed the tag:\n%s", out)
	}
	if got := ro.installedVersion("pkg", "pkg"); got != "1.0.0" {
		t.Fatalf("read token installed %s, want 1.0.0", got)
	}
}

func TestNpm_ScopedMachineTokenCannotInstallFromAnotherRepository(t *testing.T) {
	s := startNpmServe(t)
	newNpm(t, s, npmRepo, bootstrapToken).publish("pkg", "1.0.0")
	newNpm(t, s, npmOther, bootstrapToken).publish("pkg", "1.0.0")

	tok := s.issueToken(`{"name":"ci-other","repository":"` + npmOther + `","role":"read"}`)
	if got := newNpm(t, s, npmOther, tok.Token).installedVersion("pkg", "pkg"); got != "1.0.0" {
		t.Fatalf("token in its own repository installed %s, want 1.0.0", got)
	}
	newNpm(t, s, npmRepo, tok.Token).mustFail("E404", t.TempDir(), "install", "pkg")
}

func TestNpm_WhoamiWithStoredToken(t *testing.T) {
	s := startNpmServe(t)
	machine := s.issueToken(`{"name":"ci-whoami","repository":"` + npmRepo + `","role":"read"}`)

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"Bootstrap", bootstrapToken, "bootstrap"},
		{"MachineToken", machine.Token, "token:ci-whoami"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := newNpm(t, s, npmRepo, tt.token).mustRun(t.TempDir(), "whoami")
			if strings.TrimSpace(out) != tt.want {
				t.Fatalf("npm whoami = %q, want %q", out, tt.want)
			}
		})
	}
}

// Login goes through the verifier serve builds, not the adapter tests' fake. It
// is raw HTTP because npm login needs a terminal, but the token it hands back
// is then used by real npm, which is what login is for.
func TestNpm_LoginVerifiesThroughTheRealChain(t *testing.T) {
	s := startNpmServe(t)
	login := func(password string) response {
		body, err := json.Marshal(map[string]string{"name": "dev", "password": password})
		if err != nil {
			t.Fatal(err)
		}
		return s.request(http.MethodPut, "/npm/"+npmRepo+"/-/user/org.couchdb.user:dev", strings.NewReader(string(body)), nil)
	}

	machine := s.issueToken(`{"name":"ci-login","repository":"` + npmRepo + `","role":"write"}`)
	revoked := s.issueToken(`{"name":"ci-revoked","repository":"` + npmRepo + `","role":"write"}`)
	if r := login(revoked.Token); r.status != http.StatusCreated {
		t.Fatalf("login before revoking: %d %s, want 201", r.status, r.body)
	}
	if r := s.request(http.MethodDelete, "/api/v1/tokens/"+revoked.ID, nil, bearer(bootstrapToken)); r.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}

	tests := []struct {
		name       string
		password   string
		wantStatus int
	}{
		{"Bootstrap", bootstrapToken, http.StatusCreated},
		{"MachineToken", machine.Token, http.StatusCreated},
		{"Garbage", "not-a-token", http.StatusUnauthorized},
		{"RevokedMachineToken", revoked.Token, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := login(tt.password)
			if r.status != tt.wantStatus {
				t.Fatalf("login: %d %s, want %d", r.status, r.body, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusCreated {
				return
			}
			var got struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(r.body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Token != tt.password {
				t.Fatalf("login handed back a different token: %s", r.body)
			}
		})
	}

	// The same token login handed back publishes with real npm.
	var handedBack struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login(machine.Token).body, &handedBack); err != nil {
		t.Fatal(err)
	}
	n := newNpm(t, s, npmRepo, handedBack.Token)
	n.publish("pkg", "1.0.0")
	if got := n.installedVersion("pkg", "pkg"); got != "1.0.0" {
		t.Fatalf("token from login installed %s, want 1.0.0", got)
	}
}
