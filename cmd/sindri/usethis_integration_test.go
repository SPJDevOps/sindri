//go:build integration

package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/testsupport"
)

// The "Use this" panel prints commands for people to paste. These tests take the
// commands from the rendered pages and run what they say against a real server:
// the management calls as HTTP requests built from the curl text, and the
// client commands as the requests those clients make. A command that names a
// path the adapters do not serve fails here.

var codeBlock = regexp.MustCompile(`(?s)<pre><code>(.*?)</code></pre>`)

// uiCommands returns the command blocks of a UI page, as a person would copy
// them.
func (s *stack) uiCommands(path string) []string {
	s.t.Helper()
	r := s.request(http.MethodGet, path, nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		s.t.Fatalf("GET %s: %d %s", path, r.status, r.body)
	}
	var out []string
	for _, m := range codeBlock.FindAllStringSubmatch(string(r.body), -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

func findCommand(t *testing.T, cmds []string, contains string) string {
	t.Helper()
	for _, c := range cmds {
		if strings.Contains(c, contains) {
			return c
		}
	}
	t.Fatalf("no command contains %q in %q", contains, cmds)
	return ""
}

// shellWords splits a command line the way a POSIX shell does for the quoting
// the panel uses: single quotes, double quotes, and backslash escapes. The
// caller's token is substituted for $SINDRI_TOKEN first, which is what the
// reader's shell does.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	line = strings.ReplaceAll(line, "$SINDRI_TOKEN", bootstrapToken)
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'':
			inWord = true
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated single quote in %q", line)
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			j := strings.IndexByte(line[i+1:], '"')
			if j < 0 {
				t.Fatalf("unterminated double quote in %q", line)
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
		case c == '\\' && i+1 < len(line):
			inWord = true
			i++
			cur.WriteByte(line[i])
		case c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// runCurl performs what a curl command line says: its method, URL, headers,
// and body. It understands only the flags the panel uses and fails on any
// other, so a new flag is a new line here.
func (s *stack) runCurl(cmd string) response {
	s.t.Helper()
	words := shellWords(s.t, cmd)
	if len(words) == 0 || words[0] != "curl" {
		s.t.Fatalf("not a curl command: %q", cmd)
	}
	method, target, body := http.MethodGet, "", ""
	headers := http.Header{}
	for i := 1; i < len(words); i++ {
		switch w := words[i]; {
		case w == "-fsS":
		case w == "-X":
			i++
			method = words[i]
		case w == "-H":
			i++
			k, v, _ := strings.Cut(words[i], ": ")
			headers.Set(k, v)
		case w == "-d":
			i++
			body = words[i]
		case strings.HasPrefix(w, "http"):
			target = w
		default:
			s.t.Fatalf("curl flag %q is not one the panel uses: %q", w, cmd)
		}
	}
	if !strings.HasPrefix(target, s.base+"/") {
		s.t.Fatalf("the command targets %q, not this server %s", target, s.base)
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	return s.request(method, strings.TrimPrefix(target, s.base), rd, func(r *http.Request) {
		for k, v := range headers {
			r.Header[k] = v
		}
	})
}

func useStack(t *testing.T) *stack {
	t.Helper()
	addr := freeAddr(t)
	body := fmt.Sprintf("publicUrl: http://%s\nscanning:\n  enabled: true\n  trivy: %q\n  cacheDir: %q\n%slifecycle:\n  stages: [dev, prod]\n",
		addr, trivyStub(t), t.TempDir(), scanRepos)
	return startServeOn(t, addr, testsupport.DatabaseURL(t), "", body, scanHooks)
}

func TestUsePanel_ManagementCommandsRunAgainstTheServer(t *testing.T) {
	s := useStack(t)
	digest := s.push("docker/demo/app", "1.0", "use-panel")
	page := "/ui/r/docker/-/artifact?image=demo%2Fapp&tag=1.0&digest=" + url.QueryEscape(digest)

	// Promote: the page names the first stage the repository has not approved.
	promote := findCommand(t, s.uiCommands(page), "/promote")
	if !strings.Contains(promote, `"stage":"dev"`) {
		t.Fatalf("promote command %q does not name the dev stage", promote)
	}
	if r := s.runCurl(promote); r.status != http.StatusCreated || !strings.Contains(string(r.body), `"action":"approve"`) {
		t.Fatalf("promote: %d %s", r.status, r.body)
	}

	// Having approved dev, the page revokes dev and promotes into prod.
	cmds := s.uiCommands(page)
	revoke := findCommand(t, cmds, "/revoke")
	if !strings.Contains(revoke, `"stage":"dev"`) {
		t.Fatalf("revoke command %q does not name the approved stage", revoke)
	}
	if next := findCommand(t, cmds, "/promote"); !strings.Contains(next, `"stage":"prod"`) {
		t.Fatalf("promote command %q does not move to prod", next)
	}
	scan := findCommand(t, cmds, "/scan")
	if r := s.runCurl(scan); r.status != http.StatusAccepted {
		t.Fatalf("re-scan: %d %s", r.status, r.body)
	}
	if r := s.runCurl(revoke); r.status != http.StatusCreated || !strings.Contains(string(r.body), `"action":"revoke"`) {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	// Revoked is the new state: the page now has nothing to revoke from dev
	// and offers dev again.
	if again := findCommand(t, s.uiCommands(page), "/promote"); !strings.Contains(again, `"stage":"dev"`) {
		t.Fatalf("after revoking, promote is %q, want dev again", again)
	}
}

func TestUsePanel_OCICommandsNameRoutesTheRegistryServes(t *testing.T) {
	s := useStack(t)
	digest := s.push("docker/demo/app", "1.0", "use-oci")
	cmds := s.uiCommands("/ui/r/docker/-/artifact?image=demo%2Fapp&tag=1.0&digest=" + url.QueryEscape(digest))
	host := strings.TrimPrefix(s.base, "http://")

	// docker pull host/repo/image@digest and :tag are manifest GETs at
	// /v2/{repo}/{image}/manifests/{reference}.
	for _, tc := range []struct{ want, reference string }{
		{"docker pull " + host + "/docker/demo/app@" + digest, digest},
		{"docker pull " + host + "/docker/demo/app:1.0", "1.0"},
	} {
		cmd := findCommand(t, cmds, tc.want)
		ref := strings.Fields(cmd)[2]
		name, _, _ := strings.Cut(strings.TrimPrefix(ref, host+"/"), "@")
		name, _, _ = strings.Cut(name, ":")
		r := s.request(http.MethodGet, "/v2/"+name+"/manifests/"+tc.reference, nil, func(req *http.Request) {
			bearer(bootstrapToken)(req)
			req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
		})
		if r.status != http.StatusOK || r.header.Get("Docker-Content-Digest") != digest {
			t.Errorf("%q: GET manifest by %s: %d, digest %q", cmd, tc.reference, r.status, r.header.Get("Docker-Content-Digest"))
		}
	}

	// docker login host: the registry answers /v2/ to Basic with the token as
	// the password and any user name.
	login := findCommand(t, s.uiCommands("/ui/r/docker"), "docker login")
	if !strings.Contains(login, "docker login "+host+" ") {
		t.Fatalf("login command %q does not name %s", login, host)
	}
	r := s.request(http.MethodGet, "/v2/", nil, func(req *http.Request) { req.SetBasicAuth("token", bootstrapToken) })
	if r.status != http.StatusOK {
		t.Errorf("GET /v2/ with the login's credentials: %d", r.status)
	}
	if r := s.request(http.MethodGet, "/v2/", nil, nil); r.status != http.StatusUnauthorized {
		t.Errorf("GET /v2/ with no credentials: %d, want 401: the login command would be pointless", r.status)
	}
}

func TestUsePanel_PyPICommandsNameRoutesTheIndexServes(t *testing.T) {
	s := useStack(t)
	wheel := useWheel(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", "scanpkg")
	_ = mw.WriteField("version", "1.0.0")
	part, err := mw.CreateFormFile("content", "scanpkg-1.0.0-py3-none-any.whl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(wheel)
	_ = mw.Close()
	if r := s.request(http.MethodPost, "/pypi/"+scanPyPIRepo+"/upload", &body, func(req *http.Request) {
		req.SetBasicAuth("token", bootstrapToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())
	}); r.status != http.StatusOK && r.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}

	pkg := s.request(http.MethodGet, "/ui/r/py/-/package?name=scanpkg", nil, bearer(bootstrapToken))
	link := regexp.MustCompile(`href="(/ui/r/py/-/artifact\?[^"]*)"`).FindStringSubmatch(string(pkg.body))
	if link == nil {
		t.Fatalf("the package page links no file:\n%s", pkg.body)
	}
	artifact := html.UnescapeString(link[1])

	for _, client := range []string{"pip", "uv"} {
		cmds := s.uiCommands(artifact + "&client=" + client)
		cmd := findCommand(t, cmds, client+" ")
		if !strings.HasSuffix(cmd, " scanpkg==1.0.0") {
			t.Errorf("%s: command %q does not pin the version of this file", client, cmd)
		}
		// The index URL carries the token as the password. Asking it for the
		// project, as the client does, lists the file.
		words := shellWords(t, cmd)
		var index string
		for i, w := range words {
			if w == "--index-url" {
				index = words[i+1]
			}
		}
		u, err := url.Parse(index)
		if err != nil || u.User == nil {
			t.Fatalf("%s: index URL %q", client, index)
		}
		password, _ := u.User.Password()
		if u.Host != strings.TrimPrefix(s.base, "http://") || password != bootstrapToken {
			t.Errorf("%s: index URL %q does not point at this server with the token", client, index)
		}
		r := s.request(http.MethodGet, u.Path+"scanpkg/", nil, func(req *http.Request) { req.SetBasicAuth(u.User.Username(), password) })
		if r.status != http.StatusOK || !strings.Contains(string(r.body), "scanpkg-1.0.0-py3-none-any.whl") {
			t.Errorf("%s: GET %sscanpkg/: %d\n%s", client, u.Path, r.status, r.body)
		}
	}

	// The pip.conf block names the same index.
	conf := findCommand(t, s.uiCommands("/ui/r/py"), "[global]")
	if !strings.Contains(conf, "index-url = http://token:YOUR_TOKEN@"+strings.TrimPrefix(s.base, "http://")+"/pypi/py/simple/") {
		t.Errorf("pip.conf block: %q", conf)
	}
}

func TestUsePanel_NPMCommandsNameRoutesTheRegistryServes(t *testing.T) {
	s := useStack(t)
	const name, version = "scanned", "1.0.0"
	tgz := gzipTar(t, name, version)
	integrity, shasum := npmIntegrity(tgz)
	doc, err := json.Marshal(map[string]any{
		"_id": name, "name": name,
		"dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{
			"_id": name + "@" + version, "name": name, "version": version,
			"dist": map[string]string{"integrity": integrity, "shasum": shasum, "tarball": "http://client.guess/" + name + "-" + version + ".tgz"},
		}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := s.request(http.MethodPut, "/npm/"+scanNPMRepo+"/"+name, bytes.NewReader(doc), bearer(bootstrapToken)); r.status != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.status, r.body)
	}

	pkg := s.request(http.MethodGet, "/ui/r/npm/-/package?name="+name, nil, bearer(bootstrapToken))
	link := regexp.MustCompile(`href="(/ui/r/npm/-/artifact\?[^"]*)"`).FindStringSubmatch(string(pkg.body))
	if link == nil {
		t.Fatalf("the package page links no file:\n%s", pkg.body)
	}
	artifact := html.UnescapeString(link[1])
	host := strings.TrimPrefix(s.base, "http://")

	for _, client := range []string{"npm", "pnpm", "yarn"} {
		cmd := findCommand(t, s.uiCommands(artifact+"&client="+client), name+"@"+version)
		words := shellWords(t, cmd)
		registry := words[len(words)-1]
		if registry != s.base+"/npm/npm/" {
			t.Fatalf("%s: registry %q is not this server's npm repository", client, registry)
		}
		// Every client asks the registry for the package document.
		r := s.request(http.MethodGet, "/npm/npm/"+name, nil, bearer(bootstrapToken))
		if r.status != http.StatusOK || !strings.Contains(string(r.body), `"`+version+`"`) {
			t.Errorf("%s: GET packument: %d", client, r.status)
		}
	}

	// The .npmrc token line is filed under the registry URL without its
	// scheme, which is how npm matches it to requests.
	rc := s.uiCommands("/ui/r/npm?client=npm")
	if findCommand(t, rc, "registry=") != "registry="+s.base+"/npm/npm/" ||
		findCommand(t, rc, "_authToken") != "//"+host+"/npm/npm/:_authToken=${SINDRI_TOKEN}" {
		t.Errorf(".npmrc blocks: %q", rc)
	}
}

// useWheel builds a wheel the PyPI adapter accepts.
func useWheel(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"scanpkg-1.0.0.dist-info/METADATA": "Name: scanpkg\nVersion: 1.0.0\n",
		"scanpkg/__init__.py":              "x = 1\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// No page the panel appears on carries the credential the caller used.
func TestUsePanel_NoPageRendersTheBootstrapToken(t *testing.T) {
	s := useStack(t)
	digest := s.push("docker/demo/app", "1.0", "use-secrets")
	paths := []string{
		"/ui/", "/ui/system", "/ui/system/notifications",
		"/ui/r/docker", "/ui/r/docker/-/package?name=demo%2Fapp",
		"/ui/r/docker/-/artifact?image=demo%2Fapp&tag=1.0&digest=" + url.QueryEscape(digest),
		"/ui/r/py", "/ui/r/npm", "/ui/r/nope",
	}
	for _, p := range paths {
		for _, client := range []string{"", "podman", "crane", "pip", "uv", "poetry", "pnpm", "yarn"} {
			path := p
			if client != "" {
				sep := "?"
				if strings.Contains(p, "?") {
					sep = "&"
				}
				path += sep + "client=" + client
			}
			r := s.request(http.MethodGet, path, nil, func(req *http.Request) {
				bearer(bootstrapToken)(req)
				req.Header.Set("Cookie", "sindri_session="+bootstrapToken)
			})
			if strings.Contains(string(r.body), bootstrapToken) {
				t.Fatalf("GET %s renders the caller's token", path)
			}
		}
	}
}
