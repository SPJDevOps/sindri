package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

const testDigest = "sha256:4b825dc642cb6eb9a060e54bf8d69288fbee4904cafebabedeadbeefcafebabe"

func texts(steps []cmdStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Text)
	}
	return out
}

func TestPullCommands_NameTheArtifactTheWayEachClientAddressesIt(t *testing.T) {
	oci := func(base, repo string, kind model.RepoType, pkg, tag, client string) cmdInput {
		return cmdInput{Base: base, Repo: repo, Protocol: model.ProtocolOCI, Kind: kind, Package: pkg, Digest: testDigest, Tag: tag, Client: client}
	}
	const secure = "https://sindri.example.com"
	tests := []struct {
		name string
		in   cmdInput
		want []string
	}{
		{"DockerByDigest", oci(secure, "team/app", model.RepoTypeHosted, "web", "", ""),
			[]string{"docker pull sindri.example.com/team/app/web@" + testDigest}},
		{"DockerByDigestAndTag", oci(secure, "team/app", model.RepoTypeHosted, "web", "1.0", ""),
			[]string{"docker pull sindri.example.com/team/app/web@" + testDigest, "docker pull sindri.example.com/team/app/web:1.0"}},
		{"RootImageIsTheRepositoryItself", oci(secure, "team/app", model.RepoTypeHosted, "_", "", ""),
			[]string{"docker pull sindri.example.com/team/app@" + testDigest}},
		{"EmptyImageIsTheRepositoryItself", oci(secure, "team/app", model.RepoTypeHosted, "", "", ""),
			[]string{"docker pull sindri.example.com/team/app@" + testDigest}},
		{"PodmanOverPlainHTTPSkipsTLSVerify", oci("http://localhost:8080", "demo/app", model.RepoTypeHosted, "", "", "podman"),
			[]string{"podman pull --tls-verify=false localhost:8080/demo/app@" + testDigest}},
		{"CranePullsToAFile", oci(secure, "demo/app", model.RepoTypeHosted, "", "", "crane"),
			[]string{"crane pull sindri.example.com/demo/app@" + testDigest + " image.tar"}},
		{"CraneOverPlainHTTPIsInsecure", oci("http://localhost:8080", "demo/app", model.RepoTypeHosted, "", "", "crane"),
			[]string{"crane pull --insecure localhost:8080/demo/app@" + testDigest + " image.tar"}},
		{"ProxyPullsThroughTheProxy", oci(secure, "hub", model.RepoTypeProxy, "library/alpine", "", ""),
			[]string{"docker pull sindri.example.com/hub/library/alpine@" + testDigest}},
		{"UnknownClientFallsBackToDocker", oci(secure, "demo/app", model.RepoTypeHosted, "", "", "rkt"),
			[]string{"docker pull sindri.example.com/demo/app@" + testDigest}},
		{"AnUnsafeNameYieldsNothing", oci(secure, "demo/app;rm -rf", model.RepoTypeHosted, "", "", ""), nil},
		{"AnUnsafeHostYieldsNothing", oci("https://evil.example.com;id", "demo/app", model.RepoTypeHosted, "", "", ""), nil},
		{"APathOnTheBaseYieldsNothing", oci("https://sindri.example.com/prefix", "demo/app", model.RepoTypeHosted, "", "", ""), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := texts(PullCommands(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("PullCommands = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstallCommands_PerProtocolAndClient(t *testing.T) {
	pypi := func(client, ver, pkg, base string, kind model.RepoType) cmdInput {
		return cmdInput{Base: base, Repo: "internal", Protocol: model.ProtocolPyPI, Kind: kind, Package: pkg, Version: ver, Client: client}
	}
	npm := func(client, name, ver string) cmdInput {
		return cmdInput{Base: "https://sindri.example.com", Repo: "js", Protocol: model.ProtocolNPM, Kind: model.RepoTypeHosted, Package: name, Version: ver, Client: client}
	}
	const idx = `"https://token:$SINDRI_TOKEN@sindri.example.com/pypi/internal/simple/"`
	const secure = "https://sindri.example.com"
	tests := []struct {
		name string
		in   cmdInput
		want []string
	}{
		{"PipPinned", pypi("pip", "1.0", "demo", secure, model.RepoTypeHosted),
			[]string{"pip install --index-url " + idx + " demo==1.0"}},
		{"PipUnpinned", pypi("", "", "demo", secure, model.RepoTypeHosted),
			[]string{"pip install --index-url " + idx + " demo"}},
		{"PipOverPlainHTTPTrustsTheHost", pypi("pip", "1.0", "demo", "http://127.0.0.1:8080", model.RepoTypeHosted),
			[]string{`pip install --index-url "http://token:$SINDRI_TOKEN@127.0.0.1:8080/pypi/internal/simple/" --trusted-host 127.0.0.1:8080 demo==1.0`}},
		{"PipThroughAProxy", pypi("pip", "1.0", "demo", secure, model.RepoTypeProxy),
			[]string{"pip install --index-url " + idx + " demo==1.0"}},
		{"Uv", pypi("uv", "1.0", "demo", secure, model.RepoTypeHosted),
			[]string{"uv pip install --index-url " + idx + " demo==1.0"}},
		{"Poetry", pypi("poetry", "1.0", "demo", secure, model.RepoTypeHosted),
			[]string{`poetry source add --priority=primary sindri "https://sindri.example.com/pypi/internal/simple/"`,
				`poetry config http-basic.sindri token "$SINDRI_TOKEN"`, "poetry add demo==1.0"}},
		{"NpmPinned", npm("npm", "@acme/ui", "1.1.0"),
			[]string{"npm install @acme/ui@1.1.0 --registry https://sindri.example.com/npm/js/"}},
		{"NpmUnpinned", npm("", "left-pad", ""),
			[]string{"npm install left-pad --registry https://sindri.example.com/npm/js/"}},
		{"Pnpm", npm("pnpm", "@acme/ui", "1.1.0"),
			[]string{"pnpm add @acme/ui@1.1.0 --registry https://sindri.example.com/npm/js/"}},
		{"Yarn", npm("yarn", "@acme/ui", "1.1.0"),
			[]string{"yarn add @acme/ui@1.1.0 --registry https://sindri.example.com/npm/js/"}},
		{"NpmVirtualRepositoryNamedWithASlash", cmdInput{Base: secure, Repo: "team/js", Protocol: model.ProtocolNPM, Kind: model.RepoTypeVirtual, Package: "left-pad"},
			[]string{"npm install left-pad --registry https://sindri.example.com/npm/team/js/"}},
		{"OCIPackagePulledByTag", cmdInput{Base: secure, Repo: "team/app", Protocol: model.ProtocolOCI, Kind: model.RepoTypeHosted, Package: "web", Tag: "1.0"},
			[]string{"docker pull sindri.example.com/team/app/web:1.0"}},
		{"NoPackageYieldsNothing", pypi("pip", "1.0", "", secure, model.RepoTypeHosted), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := texts(InstallCommands(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("InstallCommands =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestSetupCommands_PerProtocolClientAndKind(t *testing.T) {
	const secure, plain = "https://sindri.example.com", "http://localhost:8080"
	in := func(base, repo string, p model.Protocol, k model.RepoType, client string) cmdInput {
		return cmdInput{Base: base, Repo: repo, Protocol: p, Kind: k, Client: client}
	}
	tests := []struct {
		name string
		in   cmdInput
		want []string
	}{
		{"DockerHosted", in(secure, "team/app", model.ProtocolOCI, model.RepoTypeHosted, "docker"),
			[]string{`echo "$SINDRI_TOKEN" | docker login sindri.example.com -u token --password-stdin`, "docker pull sindri.example.com/team/app/IMAGE:TAG"}},
		{"DockerVirtual", in(plain, "all", model.ProtocolOCI, model.RepoTypeVirtual, ""),
			[]string{`echo "$SINDRI_TOKEN" | docker login localhost:8080 -u token --password-stdin`, "docker pull localhost:8080/all/IMAGE:TAG"}},
		{"PodmanPlainHTTP", in(plain, "all", model.ProtocolOCI, model.RepoTypeHosted, "podman"),
			[]string{`echo "$SINDRI_TOKEN" | podman login localhost:8080 -u token --password-stdin --tls-verify=false`, "podman pull --tls-verify=false localhost:8080/all/IMAGE:TAG"}},
		{"CraneProxy", in(secure, "hub", model.ProtocolOCI, model.RepoTypeProxy, "crane"),
			[]string{`echo "$SINDRI_TOKEN" | crane auth login sindri.example.com -u token --password-stdin`, "crane pull sindri.example.com/hub/IMAGE:TAG image.tar"}},
		{"PipConfSecure", in(secure, "python", model.ProtocolPyPI, model.RepoTypeVirtual, "pip"),
			[]string{"[global]\nindex-url = https://token:YOUR_TOKEN@sindri.example.com/pypi/python/simple/"}},
		{"PipConfPlainHTTPTrustsHost", in(plain, "internal", model.ProtocolPyPI, model.RepoTypeHosted, "pip"),
			[]string{"[global]\nindex-url = http://token:YOUR_TOKEN@localhost:8080/pypi/internal/simple/\ntrusted-host = localhost:8080"}},
		{"UvToml", in(secure, "python", model.ProtocolPyPI, model.RepoTypeVirtual, "uv"),
			[]string{"[[index]]\nurl = \"https://token:YOUR_TOKEN@sindri.example.com/pypi/python/simple/\"\ndefault = true"}},
		{"Poetry", in(secure, "python", model.ProtocolPyPI, model.RepoTypeVirtual, "poetry"),
			[]string{`poetry source add --priority=primary sindri "https://sindri.example.com/pypi/python/simple/"`, `poetry config http-basic.sindri token "$SINDRI_TOKEN"`}},
		{"NpmrcKeepsTheTokenInTheUserFile", in(plain, "js", model.ProtocolNPM, model.RepoTypeHosted, "npm"),
			[]string{"registry=http://localhost:8080/npm/js/", "//localhost:8080/npm/js/:_authToken=${SINDRI_TOKEN}"}},
		{"PnpmReadsTheSameFiles", in(secure, "javascript", model.ProtocolNPM, model.RepoTypeVirtual, "pnpm"),
			[]string{"registry=https://sindri.example.com/npm/javascript/", "//sindri.example.com/npm/javascript/:_authToken=${SINDRI_TOKEN}"}},
		{"YarnRcPlainHTTPWhitelistsTheHost", in(plain, "javascript", model.ProtocolNPM, model.RepoTypeVirtual, "yarn"),
			[]string{"npmRegistryServer: \"http://localhost:8080/npm/javascript/\"\nunsafeHttpWhitelist:\n  - localhost\nnpmAlwaysAuth: true\nnpmAuthToken: \"${SINDRI_TOKEN}\""}},
		{"YarnRcSecure", in(secure, "javascript", model.ProtocolNPM, model.RepoTypeProxy, "yarn"),
			[]string{"npmRegistryServer: \"https://sindri.example.com/npm/javascript/\"\nnpmAlwaysAuth: true\nnpmAuthToken: \"${SINDRI_TOKEN}\""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := texts(SetupCommands(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("SetupCommands =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestManageCommands_CarryTheDigestStageAndBearerHeader(t *testing.T) {
	curl := func(base, action, body string) string {
		cmd := "curl -fsS -X POST " + base + "/api/v1/subjects/" + testDigest + "/" + action + ` -H "Authorization: Bearer $SINDRI_TOKEN"`
		if body != "" {
			cmd += ` -H "Content-Type: application/json" -d '` + body + `'`
		}
		return cmd
	}
	body := func(repo, stage string) string {
		return `{"repository":"` + repo + `","stage":"` + stage + `","reason":"from the web interface"}`
	}
	tests := []struct {
		name string
		in   manageInput
		want []string
	}{
		{"AllThree", manageInput{Base: "https://sindri.example.com", Repo: "team/app", Digest: testDigest, PromoteTo: "prod", RevokeFrom: "dev", Rescan: true}, []string{
			curl("https://sindri.example.com", "promote", body("team/app", "prod")),
			curl("https://sindri.example.com", "revoke", body("team/app", "dev")),
			curl("https://sindri.example.com", "scan", ""),
		}},
		{"NoScanningNoRescan", manageInput{Base: "http://localhost:8080", Repo: "demo/app", Digest: testDigest, PromoteTo: "dev", RevokeFrom: "dev"}, []string{
			curl("http://localhost:8080", "promote", body("demo/app", "dev")),
			curl("http://localhost:8080", "revoke", body("demo/app", "dev")),
		}},
		{"StageWithAQuoteIsEscapedForTheShell", manageInput{Base: "http://localhost:8080", Repo: "demo/app", Digest: testDigest, PromoteTo: "it's", RevokeFrom: "x"}, []string{
			curl("http://localhost:8080", "promote", body("demo/app", `it'\''s`)),
			curl("http://localhost:8080", "revoke", body("demo/app", "x")),
		}},
		{"NoStagesLeavesAPlaceholder", manageInput{Base: "http://localhost:8080", Repo: "demo/app", Digest: testDigest}, []string{
			curl("http://localhost:8080", "promote", body("demo/app", "STAGE")),
			curl("http://localhost:8080", "revoke", body("demo/app", "STAGE")),
		}},
		{"NoDigestYieldsNothing", manageInput{Base: "http://localhost:8080", Repo: "demo/app"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := texts(ManageCommands(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("ManageCommands =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestPickClient_UnknownFallsBackToTheDefault(t *testing.T) {
	tests := []struct {
		p          model.Protocol
		in, wanted string
	}{
		{model.ProtocolOCI, "", "docker"}, {model.ProtocolOCI, "crane", "crane"}, {model.ProtocolOCI, "pip", "docker"},
		{model.ProtocolPyPI, "poetry", "poetry"}, {model.ProtocolPyPI, "npm", "pip"},
		{model.ProtocolNPM, "yarn", "yarn"}, {model.ProtocolNPM, "<script>", "npm"},
	}
	for _, tc := range tests {
		if got := pickClient(tc.p, tc.in); got != tc.wanted {
			t.Errorf("pickClient(%s, %q) = %q, want %q", tc.p, tc.in, got, tc.wanted)
		}
	}
}

func TestInstallNotes_SayWhatTheCommandLeavesToConfiguration(t *testing.T) {
	npm := cmdInput{Base: "https://s.example.com", Repo: "js", Protocol: model.ProtocolNPM}
	if got := strings.Join(installNotes(npm), " "); !strings.Contains(got, "~/.npmrc") {
		t.Errorf("npm notes = %q, want a pointer to ~/.npmrc", got)
	}
	oci := cmdInput{Base: "http://registry.internal:8080", Repo: "r", Protocol: model.ProtocolOCI}
	if got := strings.Join(installNotes(oci), " "); !strings.Contains(got, "insecure-registries") {
		t.Errorf("plain http oci notes = %q, want the insecure-registries caution", got)
	}
	local := cmdInput{Base: "http://localhost:8080", Repo: "r", Protocol: model.ProtocolOCI}
	if got := strings.Join(installNotes(local), " "); strings.Contains(got, "insecure-registries") {
		t.Errorf("loopback notes = %q, want no insecure-registries caution", got)
	}
}
