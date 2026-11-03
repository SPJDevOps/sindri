//go:build integration

package main

import (
	"crypto/sha1" //nolint:gosec // G505: npm's dist.shasum, which the legacy package in this stub has and nothing stronger
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/testsupport"
)

const npmProxyRepo = "npmjs"

// npmUpstream is a stub public registry, serving what real npm packed.
type npmUpstream struct {
	mu        sync.Mutex
	srv       *httptest.Server
	documents map[string]map[string]any
	files     map[string][]byte
	requests  int
	asked     map[string]int
}

func newNpmUpstream(t *testing.T) *npmUpstream {
	t.Helper()
	u := &npmUpstream{documents: map[string]map[string]any{}, files: map[string][]byte{}, asked: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.requests++
		u.asked[r.RequestURI]++
		if doc, ok := u.documents[r.RequestURI]; ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		if body, ok := u.files[r.RequestURI]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// addVersion lists name@version with the dist given. The tarball is served at a
// path of the stub's own unless body is nil, and a client never sees this URL.
func (u *npmUpstream) addVersion(documentURI, name, version string, body []byte, dist map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	base := name[strings.LastIndex(name, "/")+1:]
	fileURI := "/files/" + name + "/-/" + base + "-" + version + ".tgz"
	if body != nil {
		u.files[fileURI] = body
	}
	dist["tarball"] = u.srv.URL + fileURI
	doc, ok := u.documents[documentURI]
	if !ok {
		doc = map[string]any{"name": name, "dist-tags": map[string]string{}, "versions": map[string]any{}}
		u.documents[documentURI] = doc
	}
	doc["versions"].(map[string]any)[version] = map[string]any{"name": name, "version": version, "dist": dist}
}

func (u *npmUpstream) setLatest(documentURI, version string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.documents[documentURI]["dist-tags"] = map[string]string{"latest": version}
}

func (u *npmUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

func (u *npmUpstream) askedFor(uri string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.asked[uri]
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a file the test's npm wrote
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Real npm installs through a proxy from a stub upstream, then installs again from
// a clean npm cache with the upstream gone and the packument past its TTL. Nothing
// on the client holds a copy and nothing is left upstream, so only Sindri can have
// answered. A version nobody fetched then fails, which is the proof that the first
// install really went through upstream rather than around it.
func TestNpm_InstallsThroughAProxyAndAgainWithTheUpstreamDown(t *testing.T) {
	up := newNpmUpstream(t)
	addr := freeAddr(t)
	repos := fmt.Sprintf("repos:\n  %s:\n    protocol: npm\n    upstream: { url: %q, ttl: 1s, negativeTtl: 1s }\n", npmProxyRepo, up.srv.URL)
	s := startServeOn(t, addr, testsupport.DatabaseURL(t), "", "publicUrl: http://"+addr+"\n"+repos, nil)
	n := newNpm(t, s, npmProxyRepo, bootstrapToken)

	scopedPath, scopedIntegrity := packIntegrity(t, n, writeNpmPackage(t, "@acme/demo", "1.0.0"))
	scoped := readTestFile(t, scopedPath)
	up.addVersion("/@acme%2fdemo", "@acme/demo", "1.0.0", scoped, map[string]any{"integrity": scopedIntegrity})
	up.setLatest("/@acme%2fdemo", "1.0.0")
	uncachedPath, uncachedIntegrity := packIntegrity(t, n, writeNpmPackage(t, "@acme/demo", "2.0.0"))
	up.addVersion("/@acme%2fdemo", "@acme/demo", "2.0.0", readTestFile(t, uncachedPath), map[string]any{"integrity": uncachedIntegrity})
	// Bytes that are not what the packument promises.
	_, promised := packIntegrity(t, n, writeNpmPackage(t, "@acme/demo", "3.0.0"))
	up.addVersion("/@acme%2fdemo", "@acme/demo", "3.0.0", scoped, map[string]any{"integrity": promised})

	// A legacy package: an uppercase name and only a sha1, as a 2014 package has.
	legacyPath, _ := packIntegrity(t, n, writeNpmPackage(t, "JSONStream", "1.0.0"))
	legacy := readTestFile(t, legacyPath)
	sum := sha1.Sum(legacy) //nolint:gosec // G401: as at the import
	up.addVersion("/JSONStream", "JSONStream", "1.0.0", legacy, map[string]any{"shasum": hex.EncodeToString(sum[:])})
	up.setLatest("/JSONStream", "1.0.0")

	first, _ := n.install("@acme/demo", "JSONStream")
	if up.requestCount() == 0 {
		t.Fatal("the first install never reached the upstream")
	}
	for _, name := range []string{"@acme/demo", "JSONStream"} {
		if _, err := os.Stat(filepath.Join(first, "node_modules", name, "package.json")); err != nil {
			t.Fatalf("first install of %s: %v", name, err)
		}
	}
	var lock struct {
		Packages map[string]struct {
			Resolved  string `json:"resolved"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(readTestFile(t, filepath.Join(first, "package-lock.json")), &lock); err != nil {
		t.Fatal(err)
	}
	entry := lock.Packages["node_modules/@acme/demo"]
	if want := "http://" + addr + "/npm/" + npmProxyRepo + "/@acme/demo/-/demo-1.0.0.tgz"; entry.Resolved != want {
		t.Fatalf("resolved = %q, want %q", entry.Resolved, want)
	}
	if entry.Integrity != scopedIntegrity {
		t.Fatalf("integrity = %q, want the sha512 of the packed bytes %q", entry.Integrity, scopedIntegrity)
	}

	// Bad bytes reach no node_modules, and are not cached for the next install.
	badDir := t.TempDir()
	if out, err := n.run(badDir, "install", "@acme/demo@3.0.0", "--fetch-retries=0"); err == nil {
		t.Fatalf("installing bytes that fail their integrity succeeded:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(badDir, "node_modules", "@acme", "demo")); err == nil {
		t.Fatal("bytes that fail their integrity reached node_modules")
	}

	up.srv.Close()
	time.Sleep(1200 * time.Millisecond) // past the packument's TTL, so it is served stale

	second, _ := n.install("@acme/demo", "JSONStream")
	for _, name := range []string{"@acme/demo", "JSONStream"} {
		if _, err := os.Stat(filepath.Join(second, "node_modules", name, "package.json")); err != nil {
			t.Fatalf("install with the upstream down, %s: %v", name, err)
		}
	}
	if got := readTestFile(t, filepath.Join(second, "node_modules", "@acme", "demo", "index.js")); !strings.Contains(string(got), "@acme/demo@1.0.0") {
		t.Fatalf("installed %s", got)
	}

	n.mustFail("502", t.TempDir(), "install", "@acme/demo@2.0.0", "--fetch-retries=0")
}

// One virtual URL gives real npm a hosted package and a public one, and the lockfile
// names only that URL. The stub offers a hostile @acme/lib@99.0.0, newest of all, and
// a new name in the hosted scope. The hosted scope never reaches it.
func TestNpm_OneVirtualURLInstallsAHostedAndAPublicPackage(t *testing.T) {
	const virtual = "all"
	up := newNpmUpstream(t)
	addr := freeAddr(t)
	repos := fmt.Sprintf("repos:\n  %s: { protocol: npm }\n  %s:\n    protocol: npm\n    upstream: { url: %q, ttl: 1h, negativeTtl: 1h }\n  %s: { members: [%s, %s] }\n",
		npmRepo, npmProxyRepo, up.srv.URL, virtual, npmProxyRepo, npmRepo)
	s := startServeOn(t, addr, testsupport.DatabaseURL(t), "", "publicUrl: http://"+addr+"\n"+repos, nil)
	n := newNpm(t, s, virtual, bootstrapToken)

	newNpm(t, s, npmRepo, bootstrapToken).publish("@acme/lib", "1.0.0")

	publicPath, publicIntegrity := packIntegrity(t, n, writeNpmPackage(t, "left-pad", "1.3.0"))
	up.addVersion("/left-pad", "left-pad", "1.3.0", readTestFile(t, publicPath), map[string]any{"integrity": publicIntegrity})
	up.setLatest("/left-pad", "1.3.0")
	for _, name := range []string{"@acme/lib", "@acme/new"} {
		uri := "/" + strings.ReplaceAll(name, "/", "%2f")
		hostilePath, hostileIntegrity := packIntegrity(t, n, writeNpmPackage(t, name, "99.0.0"))
		up.addVersion(uri, name, "99.0.0", readTestFile(t, hostilePath), map[string]any{"integrity": hostileIntegrity})
		up.setLatest(uri, "99.0.0")
	}

	dir, _ := n.install("@acme/lib", "left-pad")
	if got := readTestFile(t, filepath.Join(dir, "node_modules", "@acme", "lib", "index.js")); !strings.Contains(string(got), "@acme/lib@1.0.0") {
		t.Fatalf("installed %s, want the hosted 1.0.0", got)
	}
	if got := readTestFile(t, filepath.Join(dir, "node_modules", "left-pad", "index.js")); !strings.Contains(string(got), "left-pad@1.3.0") {
		t.Fatalf("installed %s, want the public 1.3.0", got)
	}

	var lock struct {
		Packages map[string]struct {
			Resolved string `json:"resolved"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(readTestFile(t, filepath.Join(dir, "package-lock.json")), &lock); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"@acme/lib": "lib-1.0.0.tgz", "left-pad": "left-pad-1.3.0.tgz"} {
		want := "http://" + addr + "/npm/" + virtual + "/" + name + "/-/" + file
		if got := lock.Packages["node_modules/"+name].Resolved; got != want {
			t.Fatalf("%s resolved = %q, want %q", name, got, want)
		}
	}
	if up.askedFor("/left-pad") == 0 {
		t.Fatal("the public package never reached the upstream")
	}

	n.mustFail("ETARGET", t.TempDir(), "install", "@acme/lib@99.0.0")
	n.mustFail("E404", t.TempDir(), "install", "@acme/new")
	for _, uri := range []string{"/@acme%2flib", "/@acme%2fnew"} {
		if got := up.askedFor(uri); got != 0 {
			t.Fatalf("the hosted scope reached the upstream: %d requests for %s", got, uri)
		}
	}
}

// Through the real wiring: a token scoped to the virtual repository may address it
// and may read none of its members. The virtual repository republishes nothing, and
// a name the hosted member claims does not fall through to the proxy.
func TestNpm_VirtualRepositoryAnswersOnlyFromMembersTheCallerMayRead(t *testing.T) {
	const virtual = "all"
	up := newNpmUpstream(t)
	addr := freeAddr(t)
	repos := fmt.Sprintf("repos:\n  %s: { protocol: npm }\n  %s:\n    protocol: npm\n    upstream: { url: %q, ttl: 1h, negativeTtl: 1h }\n  %s: { members: [%s, %s] }\n",
		npmRepo, npmProxyRepo, up.srv.URL, virtual, npmRepo, npmProxyRepo)
	s := startServeOn(t, addr, testsupport.DatabaseURL(t), "", "publicUrl: http://"+addr+"\n"+repos, nil)
	newNpm(t, s, npmRepo, bootstrapToken).publish("@acme/lib", "1.0.0")

	token := s.issueToken(`{"name":"ci-virtual","repository":"` + virtual + `","role":"read"}`)
	for _, path := range []string{"@acme%2flib", "@acme/lib/-/lib-1.0.0.tgz", "@acme%2fnew", "left-pad"} {
		if r := s.request(http.MethodGet, "/npm/"+virtual+"/"+path, nil, bearer(token.Token)); r.status != http.StatusNotFound {
			t.Fatalf("GET %s = %d %s, want 404", path, r.status, r.body)
		}
	}
	if r := s.request(http.MethodGet, "/npm/"+virtual+"/@acme%2flib", nil, bearer(bootstrapToken)); r.status != http.StatusOK {
		t.Fatalf("an admin's read = %d %s, want 200", r.status, r.body)
	}
	if got := up.requestCount(); got != 0 {
		t.Fatalf("%d upstream requests on behalf of a caller who may read no member", got)
	}
}
