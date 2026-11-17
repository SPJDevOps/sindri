//go:build integration

package ui

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

const useHost = "registry.example.test"

// promote approves d in stage of repo.
func (e *env) promote(t *testing.T, d model.Digest, repo, stage string) {
	t.Helper()
	if _, err := e.lc.Promote(t.Context(), lifecycle.ChangeParams{Digest: d, Repository: repo, Stage: stage, Actor: "user:admin", Reason: "use test"}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
}

// useEnv is an environment that knows its public address.
func useEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.h.opts.PublicURL = "https://" + useHost
	return e
}

// commandTexts returns the text of every command block on a page.
func commandTexts(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<pre><code>(.*?)</code></pre>`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func hasCommand(body, want string) bool {
	for _, c := range commandTexts(body) {
		if c == want {
			return true
		}
	}
	return false
}

func publishPyPI(t *testing.T, e *env, pkg, version string) model.Digest {
	t.Helper()
	f, err := e.reg.Publish(t.Context(), registry.PublishParams{
		Repo: "py", Package: pkg, Version: version, Filename: pkg + "-" + version + ".tar.gz",
		Body: strings.NewReader("sdist " + version), Size: int64(len("sdist " + version)),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return f.Digest
}

func TestUse_OCIArtifactShowsPullByDigestAndByTagInTheHeaderArea(t *testing.T) {
	e := useEnv(t)
	d := e.push("team-a", "app", "1.0", "use")

	pkg := e.get("alice", "/ui/r/team-a/-/package?name=app")
	link := artifactLink.FindStringSubmatch(pkg.body)
	if link == nil || !strings.Contains(link[1], "tag=1.0") {
		t.Fatalf("the tag row does not link the artifact with its tag: %v", link)
	}
	byTag := "docker pull " + useHost + "/team-a/app:1.0"
	if !hasCommand(pkg.body, byTag) {
		t.Errorf("the package page lacks %q: %q", byTag, commandTexts(pkg.body))
	}

	art := e.get("alice", link[1])
	for _, want := range []string{
		"docker pull " + useHost + "/team-a/app@" + d.String(),
		byTag,
	} {
		if !hasCommand(art.body, want) {
			t.Errorf("artifact page lacks %q: %q", want, commandTexts(art.body))
		}
	}
	// The panel is part of the header area: before the facts, the lifecycle,
	// and everything else the page shows.
	if use, facts := strings.Index(art.body, `id="use-title"`), strings.Index(art.body, `class="facts"`); use < 0 || use > facts {
		t.Errorf("the panel is not above the facts (panel at %d, facts at %d)", use, facts)
	}
	if strings.Contains(art.body, "publicUrl is not set") {
		t.Error("the artifact page shows the publicUrl hint though publicUrl is set")
	}
}

func TestUse_ClientTabsLiveInTheURLAndKeepTheOtherParameters(t *testing.T) {
	e := useEnv(t)
	d := e.push("team-a", "app", "1.0", "tabs")
	path := "/ui/r/team-a/-/artifact?image=app&tag=1.0&digest=" + d.String()

	tests := []struct {
		name, client, want, current string
	}{
		{"Podman", "podman", "podman pull " + useHost + "/team-a/app@" + d.String(), "podman"},
		{"Crane", "crane", "crane pull " + useHost + "/team-a/app@" + d.String() + " image.tar", "crane"},
		{"UnknownFallsBackToDocker", "rkt%27;id", "docker pull " + useHost + "/team-a/app@" + d.String(), "docker"},
		{"AnotherProtocolsClientFallsBackToDocker", "pip", "docker pull " + useHost + "/team-a/app@" + d.String(), "docker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.get("alice", path+"&client="+tc.client)
			if r.status != http.StatusOK {
				t.Fatalf("status %d", r.status)
			}
			if !hasCommand(r.body, tc.want) {
				t.Errorf("lacks %q: %q", tc.want, commandTexts(r.body))
			}
			if !strings.Contains(r.body, `aria-current="page">`+tc.current+`</a>`) {
				t.Errorf("the current tab is not %s", tc.current)
			}
			// Every tab link keeps image, tag, and digest, and sets only client.
			tabs := regexp.MustCompile(`<a href="([^"]*client=[^"]*)"`).FindAllStringSubmatch(r.body, -1)
			if len(tabs) != 3 {
				t.Fatalf("found %d tab links, want 3", len(tabs))
			}
			for _, m := range tabs {
				u, err := url.Parse(m[1])
				if err != nil {
					t.Fatal(err)
				}
				q := u.Query()
				if q.Get("image") != "app" || q.Get("tag") != "1.0" || q.Get("digest") != d.String() || q.Get("client") == "" {
					t.Errorf("tab link %s drops a parameter", m[1])
				}
			}
		})
	}
}

func TestUse_ATagThatDoesNotNameThisDigestIsNotShown(t *testing.T) {
	e := useEnv(t)
	d1 := e.push("team-a", "app", "1.0", "one")
	e.push("team-a", "app", "2.0", "two")
	for name, tag := range map[string]string{"AnotherDigestsTag": "2.0", "NoSuchTag": "9.9", "ADigestAsTag": d1.String()} {
		t.Run(name, func(t *testing.T) {
			r := e.get("alice", "/ui/r/team-a/-/artifact?image=app&digest="+d1.String()+"&tag="+url.QueryEscape(tag))
			if r.status != http.StatusOK {
				t.Fatalf("status %d", r.status)
			}
			if !hasCommand(r.body, "docker pull "+useHost+"/team-a/app@"+d1.String()) {
				t.Error("the by-digest command is missing")
			}
			if strings.Contains(r.body, "app:"+tag) {
				t.Errorf("the page shows a pull by tag %s that does not name this digest", tag)
			}
		})
	}
}

func TestUse_ManageCommandsAreForWritersOfAHostedRepositoryOnly(t *testing.T) {
	e := useEnv(t)
	e.h.opts.DocsURL = "https://docs.example.test/sindri/"
	d := e.push("team-a", "app", "1.0", "manage")
	path := "/ui/r/team-a/-/artifact?image=app&digest=" + d.String()

	// alice reads team-a but may not write it.
	if r := e.get("alice", path); strings.Contains(r.body, "/promote") || strings.Contains(r.body, "Manage") {
		t.Error("a reader sees the management commands")
	}

	r := e.get("admin", path)
	base := "curl -fsS -X POST https://" + useHost + "/api/v1/subjects/" + d.String()
	hdr := ` -H "Authorization: Bearer $SINDRI_TOKEN"`
	body := `{"repository":"team-a","stage":"dev","reason":"from the web interface"}`
	for _, want := range []string{
		base + "/promote" + hdr + ` -H "Content-Type: application/json" -d '` + body + `'`,
	} {
		if !hasCommand(r.body, want) {
			t.Errorf("lacks %q: %q", want, commandTexts(r.body))
		}
	}
	if strings.Contains(r.body, "/scan") {
		t.Error("the re-scan command shows with scanning off")
	}
	if !strings.Contains(r.body, `<a href="https://docs.example.test/sindri/quickstart.html" rel="noreferrer noopener">how tokens work</a>`) {
		t.Error("no link to the token documentation")
	}

	e.h.opts.ScanEnabled = true
	if r := e.get("admin", path); !hasCommand(r.body, base+"/scan"+hdr) {
		t.Errorf("no re-scan command with scanning on: %q", commandTexts(r.body))
	}

	// With a stage approved, the revoke command names it and the promote
	// command moves on to the next.
	e.promote(t, d, "team-a", "dev")
	r = e.get("admin", path)
	if !hasCommand(r.body, base+"/revoke"+hdr+` -H "Content-Type: application/json" -d '`+body+`'`) {
		t.Errorf("revoke does not name the approved stage: %q", commandTexts(r.body))
	}
	prodBody := strings.Replace(body, `"dev"`, `"prod"`, 1)
	if !hasCommand(r.body, base+"/promote"+hdr+` -H "Content-Type: application/json" -d '`+prodBody+`'`) {
		t.Errorf("promote does not move to the next stage: %q", commandTexts(r.body))
	}
}

func TestUse_ProxyContentPullsThroughTheProxyAndIsNeverManaged(t *testing.T) {
	e := useEnv(t)
	e.h.opts.ScanEnabled = true
	idx := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	m, err := e.reg.PlaceCachedManifest(t.Context(), "hub", "library/alpine", "3.20", "application/vnd.oci.image.index.v1+json", idx)
	if err != nil {
		t.Fatalf("PlaceCachedManifest: %v", err)
	}
	r := e.get("admin", "/ui/r/hub/-/artifact?image=library%2Falpine&tag=3.20&digest="+m.Digest.String())
	if r.status != http.StatusOK {
		t.Fatalf("status %d\n%s", r.status, r.body)
	}
	for _, want := range []string{
		"docker pull " + useHost + "/hub/library/alpine@" + m.Digest.String(),
		"docker pull " + useHost + "/hub/library/alpine:3.20",
	} {
		if !hasCommand(r.body, want) {
			t.Errorf("lacks %q: %q", want, commandTexts(r.body))
		}
	}
	for _, banned := range []string{"/promote", "/revoke", "/scan", "Manage"} {
		if strings.Contains(r.body, banned) {
			t.Errorf("a proxy artifact page shows %q", banned)
		}
	}
	if !strings.Contains(r.body, "through the proxy") {
		t.Error("the page does not say the pull goes through the proxy")
	}
}

func TestUse_FallsBackToTheRequestAddressAndSaysSo(t *testing.T) {
	e := newEnv(t) // no publicUrl
	d := e.push("team-a", "app", "1.0", "fallback")
	path := "/ui/r/team-a/-/artifact?image=app&digest=" + d.String()

	// httptest requests carry Host example.com. The server ignores
	// X-Forwarded-*, so the page does too.
	r := e.get("alice", path, "X-Forwarded-Host", "evil.test", "X-Forwarded-Proto", "https")
	if !hasCommand(r.body, "docker pull example.com/team-a/app@"+d.String()) {
		t.Errorf("the command does not use the request address: %q", commandTexts(r.body))
	}
	if strings.Contains(r.body, "evil.test") {
		t.Error("the page trusted X-Forwarded-Host")
	}
	if !strings.Contains(r.body, "publicUrl is not set; commands use the address this page was loaded from") {
		t.Error("the page does not say publicUrl is unset")
	}

	e.h.opts.PublicURL = "https://" + useHost + "/"
	r = e.get("alice", path)
	if !hasCommand(r.body, "docker pull "+useHost+"/team-a/app@"+d.String()) || strings.Contains(r.body, "publicUrl is not set") {
		t.Errorf("publicUrl (with a trailing slash) is not used cleanly: %q", commandTexts(r.body))
	}
}

func TestUse_PyPIPackageAndArtifactShowInstallCommands(t *testing.T) {
	e := useEnv(t)
	d := publishPyPI(t, e, "demo", "1.0")
	publishPyPI(t, e, "demo", "2.0")
	idx := `"https://token:$SINDRI_TOKEN@` + useHost + `/pypi/py/simple/"`

	pkg := e.get("alice", "/ui/r/py/-/package?name=demo&client=uv")
	if want := "uv pip install --index-url " + idx + " demo"; !hasCommand(pkg.body, want) {
		t.Errorf("package page lacks %q: %q", want, commandTexts(pkg.body))
	}
	if use, versions := strings.Index(pkg.body, `id="use-title"`), strings.Index(pkg.body, "Versions"); use < 0 || use > versions {
		t.Error("the install panel is not above the version list")
	}

	var file string
	for _, m := range artifactLink.FindAllStringSubmatch(pkg.body, -1) {
		if strings.Contains(m[1], url.QueryEscape(d.String())) {
			file = m[1]
		}
	}
	if file == "" {
		t.Fatalf("no file link for %s", d)
	}
	for client, want := range map[string]string{
		"pip":    "pip install --index-url " + idx + " demo==1.0",
		"uv":     "uv pip install --index-url " + idx + " demo==1.0",
		"poetry": "poetry add demo==1.0",
	} {
		art := e.get("alice", file+"&client="+client)
		if !hasCommand(art.body, want) {
			t.Errorf("%s: artifact page lacks %q: %q", client, want, commandTexts(art.body))
		}
	}

	// A link naming a package the digest is not a file of shows no install
	// command: the page never prints a command for something it does not hold.
	forged := e.get("alice", "/ui/r/py/-/artifact?image=evil&digest="+d.String())
	if strings.Contains(forged.body, "pip install") {
		t.Error("the page printed an install command for a package the digest does not belong to")
	}
}

func TestUse_NPMPackageAndArtifactShowInstallCommands(t *testing.T) {
	e := useEnv(t)
	d := e.publishNPM("npm", "@acme/ui", "1.0.0", "")
	e.publishNPM("npm", "@acme/ui", "2.0.0-beta.1", "beta")
	reg := "https://" + useHost + "/npm/npm/"

	pkg := e.get("alice", "/ui/r/npm/-/package?name="+url.QueryEscape("@acme/ui"))
	if want := "npm install @acme/ui@1.0.0 --registry " + reg; !hasCommand(pkg.body, want) {
		t.Errorf("package page pins the latest dist-tag: lacks %q: %q", want, commandTexts(pkg.body))
	}
	var file string
	for _, m := range artifactLink.FindAllStringSubmatch(pkg.body, -1) {
		if strings.Contains(m[1], url.QueryEscape(d.String())) {
			file = m[1]
		}
	}
	for client, want := range map[string]string{
		"npm":  "npm install @acme/ui@1.0.0 --registry " + reg,
		"pnpm": "pnpm add @acme/ui@1.0.0 --registry " + reg,
		"yarn": "yarn add @acme/ui@1.0.0 --registry " + reg,
	} {
		art := e.get("alice", file+"&client="+client)
		if !hasCommand(art.body, want) {
			t.Errorf("%s: artifact page lacks %q: %q", client, want, commandTexts(art.body))
		}
	}
}

func TestUse_RepositoryPageHasClientSetupAtTheTop(t *testing.T) {
	e := useEnv(t)
	tests := []struct {
		name, path, user string
		want             []string
		before           string // the section the setup must precede
	}{
		{"HostedOCI", "/ui/r/team-a", "alice",
			[]string{`echo "$SINDRI_TOKEN" | docker login ` + useHost + ` -u token --password-stdin`}, "Images"},
		{"VirtualOCI", "/ui/r/all?client=podman", "alice",
			[]string{`echo "$SINDRI_TOKEN" | podman login ` + useHost + ` -u token --password-stdin`}, "Members"},
		{"HostedPyPI", "/ui/r/py", "alice",
			[]string{"[global]\nindex-url = https://token:YOUR_TOKEN@" + useHost + "/pypi/py/simple/"}, "Packages"},
		{"HostedNPM", "/ui/r/npm?client=pnpm", "alice",
			[]string{"registry=https://" + useHost + "/npm/npm/", "//" + useHost + "/npm/npm/:_authToken=${SINDRI_TOKEN}"}, "Packages"},
		{"VirtualNPMYarn", "/ui/r/npm-all?client=yarn", "alice",
			[]string{"npmRegistryServer: \"https://" + useHost + "/npm/npm-all/\"\nnpmAlwaysAuth: true\nnpmAuthToken: \"${SINDRI_TOKEN}\""}, "Members"},
		{"ProxyOCI", "/ui/r/hub?client=crane", "alice",
			[]string{`echo "$SINDRI_TOKEN" | crane auth login ` + useHost + ` -u token --password-stdin`}, "Images"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.get(tc.user, tc.path)
			if r.status != http.StatusOK {
				t.Fatalf("status %d", r.status)
			}
			for _, want := range tc.want {
				if !hasCommand(r.body, want) {
					t.Errorf("lacks %q: %q", want, commandTexts(r.body))
				}
			}
			if use, section := strings.Index(r.body, "Set up your client"), strings.Index(r.body, tc.before); use < 0 || use > section {
				t.Errorf("the setup is not above %s (setup at %d, section at %d)", tc.before, use, section)
			}
		})
	}
}

// No page may carry the credential the caller used. The commands name
// $SINDRI_TOKEN and YOUR_TOKEN, and nothing else; a token in a page would be a
// token in a screenshot, a proxy log, or a browser cache.
func TestUse_NoPageRendersTheCallersTokenOrSessionCookie(t *testing.T) {
	e := useEnv(t)
	const token, cookie = "sindri_SECRETTOKENVALUE0123456789", "SESSIONCOOKIEVALUE0123456789"
	d := e.push("team-a", "app", "1.0", "secrets")
	pd := publishPyPI(t, e, "demo", "1.0")
	nd := e.publishNPM("npm", "@acme/ui", "1.0.0", "")
	e.h.opts.ScanEnabled = true

	paths := []string{
		"/ui/", "/ui/system", "/ui/system/notifications",
		"/ui/r/team-a", "/ui/r/team-a/-/package?name=app",
		"/ui/r/team-a/-/artifact?image=app&tag=1.0&digest=" + d.String(),
		"/ui/r/py", "/ui/r/py/-/package?name=demo", "/ui/r/py/-/artifact?image=demo&digest=" + pd.String(),
		"/ui/r/npm", "/ui/r/npm/-/package?name=" + url.QueryEscape("@acme/ui"),
		"/ui/r/npm/-/artifact?image=" + url.QueryEscape("@acme/ui") + "&digest=" + nd.String(),
		"/ui/r/all", "/ui/r/npm-all", "/ui/r/hub", "/ui/r/npmjs", "/ui/r/nope", "/ui/elsewhere",
	}
	var clients []string
	for _, cs := range clientsByProtocol {
		clients = append(clients, cs...)
	}
	for _, path := range paths {
		for _, client := range append([]string{""}, clients...) {
			p := path
			if client != "" {
				sep := "?"
				if strings.Contains(p, "?") {
					sep = "&"
				}
				p += sep + "client=" + client
			}
			for _, user := range []string{"alice", "admin"} {
				r := e.get(user, p, "Authorization", "Bearer "+token, "Cookie", "sindri_session="+cookie)
				for _, secret := range []string{token, cookie} {
					if strings.Contains(r.body, secret) {
						t.Fatalf("GET %s as %s renders the caller's credential", p, user)
					}
				}
			}
		}
	}
}
