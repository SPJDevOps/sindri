//go:build integration

package npm

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func loginBody(password string) []byte {
	b, err := json.Marshal(map[string]string{"name": "anything", "password": password})
	if err != nil {
		panic(err) // a map of strings always encodes
	}
	return b
}

func loginPath(repo, user string) string {
	return "/npm/" + repo + "/-/user/" + userDocPrefix + user
}

// login sends a login with no credential header, the way npm does.
func (f *npmFixture) login(repo, user string, body []byte) (int, []byte, http.Header) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPut, f.ts.URL+loginPath(repo, user), bytes.NewReader(body))
	if err != nil {
		f.t.Fatalf("building request: %v", err)
	}
	res, err := f.ts.Client().Do(req)
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var got bytes.Buffer
	if _, err := got.ReadFrom(res.Body); err != nil {
		f.t.Fatalf("reading login: %v", err)
	}
	return res.StatusCode, got.Bytes(), res.Header
}

func TestLogin_ValidTokenIsReturned(t *testing.T) {
	f := newFixture(t, testPublicURL)
	status, got, _ := f.login(testRepo, "alice", loginBody(ciToken))
	if status != http.StatusCreated {
		t.Fatalf("login = %d %s, want 201", status, got)
	}
	var res struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(got, &res); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if !res.OK || res.ID != "org.couchdb.user:alice" || res.Token != ciToken {
		t.Fatalf("login response = %+v, want ok, the user document, and the token back", res)
	}

	body := publishBody(t, "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))
	if status, got, _ := f.do(res.Token, http.MethodPut, "/npm/"+testRepo+"/foo", body); status != http.StatusCreated {
		t.Fatalf("publish with the issued token = %d %s, want 201", status, got)
	}
}

func TestLogin_InvalidTokenIs401(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "team/other", Type: model.RepoTypeHosted})
	var first []byte
	for _, repo := range []string{testRepo, "team/other", "no/such-repo"} {
		t.Run(repo, func(t *testing.T) {
			status, got, header := f.login(repo, "alice", loginBody("sindri_wrong"))
			if status != http.StatusUnauthorized {
				t.Fatalf("login = %d %s, want 401", status, got)
			}
			if want := `{"error":"invalid credentials"}`; strings.TrimSpace(string(got)) != want {
				t.Fatalf("body = %s, want %s", got, want)
			}
			if header.Get("WWW-Authenticate") != "" {
				t.Fatalf("login challenged with %q, want no challenge", header.Get("WWW-Authenticate"))
			}
			if first == nil {
				first = got
			} else if !bytes.Equal(first, got) {
				t.Fatalf("body varies with the repository: %s then %s", first, got)
			}
		})
	}
}

func TestLogin_UsernameIsIgnored(t *testing.T) {
	f := newFixture(t, testPublicURL)
	for _, tc := range []struct{ name, user, bodyName string }{
		{name: "path and body differ", user: "alice", bodyName: "mallory"},
		{name: "no name in the body", user: "bob", bodyName: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"name": tc.bodyName, "password": ciToken})
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			status, got, _ := f.login(testRepo, tc.user, body)
			if status != http.StatusCreated {
				t.Fatalf("login = %d %s, want 201", status, got)
			}
		})
	}
}

func TestLogin_BodyOverLimitIsRefused(t *testing.T) {
	f := newFixture(t, testPublicURL)
	status, got, _ := f.login(testRepo, "alice", loginBody(strings.Repeat("a", maxLoginBody)))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("login = %d %s, want 413", status, got)
	}
	wantJSONError(t, got)
}

func TestLogin_MalformedBodyIs400WithoutEchoingIt(t *testing.T) {
	f := newFixture(t, testPublicURL)
	status, got, _ := f.login(testRepo, "alice", []byte(`{"password":"sindri_leaky",`))
	if status != http.StatusBadRequest {
		t.Fatalf("login = %d %s, want 400", status, got)
	}
	if strings.Contains(string(got), "sindri_leaky") {
		t.Fatalf("response echoes the body: %s", got)
	}
}

func TestLogin_OnlyPutIsAllowed(t *testing.T) {
	f := newFixture(t, testPublicURL)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			status, got, _ := f.do("", method, loginPath(testRepo, "alice"), loginBody(ciToken))
			if status != http.StatusMethodNotAllowed {
				t.Fatalf("%s = %d %s, want 405", method, status, got)
			}
		})
	}
}

func TestLogin_NeverLogsTheToken(t *testing.T) {
	var logs bytes.Buffer
	f := newFixtureLogging(t, testPublicURL, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	const wrong = "sindri_wrong-secret"
	if status, got, _ := f.login(testRepo, "alice", loginBody(ciToken)); status != http.StatusCreated {
		t.Fatalf("login = %d %s, want 201", status, got)
	}
	if status, got, _ := f.login(testRepo, "alice", loginBody(wrong)); status != http.StatusUnauthorized {
		t.Fatalf("login = %d %s, want 401", status, got)
	}
	if logs.Len() == 0 {
		t.Fatal("the server logged nothing, so this test proves nothing")
	}
	for _, secret := range []string{ciToken, wrong} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log contains %q:\n%s", secret, logs.String())
		}
	}
}

func TestWebLogin_IsAConstant404(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "team/other", Type: model.RepoTypeHosted})
	for _, repo := range []string{testRepo, "team/other", "no/such-repo"} {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
			t.Run(method+" "+repo, func(t *testing.T) {
				status, got, header := f.do("", method, "/npm/"+repo+"/-/v1/login", nil)
				if status != http.StatusNotFound {
					t.Fatalf("%s = %d %s, want 404", method, status, got)
				}
				if header.Get("WWW-Authenticate") != "" {
					t.Fatalf("web login challenged with %q", header.Get("WWW-Authenticate"))
				}
			})
		}
	}
}

func TestWhoami_ReturnsSubject(t *testing.T) {
	f := newFixture(t, testPublicURL)
	status, got, _ := f.do(ciToken, http.MethodGet, "/npm/"+testRepo+"/-/whoami", nil)
	if status != http.StatusOK {
		t.Fatalf("whoami = %d %s, want 200", status, got)
	}
	if want := `{"username":"token:ci"}`; strings.TrimSpace(string(got)) != want {
		t.Fatalf("whoami = %s, want %s", got, want)
	}
	if status, got, _ := f.do("", http.MethodGet, "/npm/"+testRepo+"/-/whoami", nil); status != http.StatusUnauthorized {
		t.Fatalf("whoami with no credential = %d %s, want 401", status, got)
	}
	if status, got, _ := f.do(outsiderToken, http.MethodGet, "/npm/"+testRepo+"/-/whoami", nil); status != http.StatusNotFound {
		t.Fatalf("whoami by a caller who cannot read = %d %s, want 404", status, got)
	}
}

func TestMachineToken_ScopedToOneRepositoryCannotReadAnother(t *testing.T) {
	f := newFixture(t, testPublicURL)
	const other = "team/other"
	f.createRepo(registry.CreateRepositoryParams{Name: other, Type: model.RepoTypeHosted})
	f.mustPublish("foo", "1.0.0")
	f.mustPublishTo(other, "foo", "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))

	paths := func(repo string) map[string]string {
		return map[string]string{
			"packument": "/npm/" + repo + "/foo",
			"tarball":   "/npm/" + repo + "/foo/-/foo-1.0.0.tgz",
			"dist-tags": "/npm/" + repo + "/-/package/foo/dist-tags",
		}
	}
	for name, path := range paths(testRepo) {
		if status, got, _ := f.do(readerToken, http.MethodGet, path, nil); status != http.StatusOK {
			t.Fatalf("%s in the scoped repository = %d %s, want 200", name, status, got)
		}
	}
	for name, path := range paths(other) {
		if status, got, _ := f.do(readerToken, http.MethodGet, path, nil); status != http.StatusNotFound {
			t.Fatalf("%s in another repository = %d %s, want 404", name, status, got)
		}
	}
}

// authCase is what one route answers to three callers: none, one who may only
// read, and one who may not read the repository.
type authCase struct {
	method string
	path   string
	body   []byte
	// noCredential, readOnly, and unreadable are the expected statuses.
	noCredential, readOnly, unreadable int
	// bypass marks a route that answers an anonymous caller itself, so the
	// middleware's Bearer challenge must be absent.
	bypass bool
}

func TestNpm_EveryRouteHasAnAuthorizationCase(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	base := "/npm/" + testRepo

	cases := map[routeKind]authCase{
		routePackument: {method: http.MethodGet, path: base + "/foo",
			noCredential: http.StatusUnauthorized, readOnly: http.StatusOK, unreadable: http.StatusNotFound},
		routeTarball: {method: http.MethodGet, path: base + "/foo/-/foo-1.0.0.tgz",
			noCredential: http.StatusUnauthorized, readOnly: http.StatusOK, unreadable: http.StatusNotFound},
		routeDistTags: {method: http.MethodPut, path: base + "/-/package/foo/dist-tags/beta", body: []byte(`"1.0.0"`),
			noCredential: http.StatusUnauthorized, readOnly: http.StatusForbidden, unreadable: http.StatusNotFound},
		routeWhoami: {method: http.MethodGet, path: base + "/-/whoami",
			noCredential: http.StatusUnauthorized, readOnly: http.StatusOK, unreadable: http.StatusNotFound},
		// The two login routes are the sanctioned exception to "every route
		// authorizes" (ADR 24): npm sends them no credential, they expose no
		// repository data, and they answer the same to every caller.
		routeWebLogin: {method: http.MethodPost, path: base + "/-/v1/login", bypass: true,
			noCredential: http.StatusNotFound, readOnly: http.StatusNotFound, unreadable: http.StatusNotFound},
		routeUserLogin: {method: http.MethodPut, path: loginPath(testRepo, "alice"), body: loginBody("sindri_wrong"), bypass: true,
			noCredential: http.StatusUnauthorized, readOnly: http.StatusUnauthorized, unreadable: http.StatusUnauthorized},
	}

	for kind := range routeKindCount {
		tc, ok := cases[kind]
		if !ok {
			t.Errorf("route kind %d has no authorization case: add one here", kind)
			continue
		}
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rt, parsed := parseRoute(strings.TrimPrefix(tc.path, "/npm/"))
			if !parsed || rt.kind != kind {
				t.Fatalf("path %s parses as %+v (ok=%v), not kind %d", tc.path, rt, parsed, kind)
			}
			for _, caller := range []struct {
				name, token string
				want        int
			}{
				{"no credential", "", tc.noCredential},
				{"read-only", readerToken, tc.readOnly},
				{"unreadable", outsiderToken, tc.unreadable},
			} {
				status, got, header := f.do(caller.token, tc.method, tc.path, tc.body)
				if status != caller.want {
					t.Errorf("%s: %s %s = %d %s, want %d", caller.name, tc.method, tc.path, status, got, caller.want)
				}
				if caller.token == "" && tc.bypass && header.Get("WWW-Authenticate") != "" {
					t.Errorf("%s: a login route challenged an anonymous caller", caller.name)
				}
			}
		})
	}
}
