package ui

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// This file is the one place that writes a command for a person to paste. The
// functions are pure: they take a cmdInput and return text, so a table test per
// protocol and repository kind pins every command, and the pages only choose
// which builder to call. Every URL shape here is the one the protocol adapter
// serves (/v2 for OCI, /pypi/{repo}/simple/ for PyPI, /npm/{repo}/ for npm,
// /api/v1/subjects/{digest}/... for the management API).
//
// No builder is given a credential. A command names $SINDRI_TOKEN, which the
// reader's shell expands, or a placeholder in a config file, so a rendered
// page never holds a token (security rule 4).

// cmdInput is everything a builder reads.
type cmdInput struct {
	// Base is the origin clients reach the server at: scheme://host[:port],
	// with no trailing slash.
	Base     string
	Repo     string
	Protocol model.Protocol
	Kind     model.RepoType
	// Package is the OCI image under the repository ("" and "_" mean the
	// repository itself), the PyPI project, or the npm package name.
	Package string
	Version string
	Digest  string
	Tag     string
	Client  string
	// Fallback records that Base came from the request and not from
	// publicUrl, which the page says. It changes no command.
	Fallback bool
}

// cmdStep is one command, or one block of configuration, with an optional
// caption above it.
type cmdStep struct {
	Caption string
	Text    string
}

// The client shown when ?client= is missing or unknown is the first of its
// protocol's list.
var clientsByProtocol = map[model.Protocol][]string{
	model.ProtocolOCI:  {"docker", "podman", "crane"},
	model.ProtocolPyPI: {"pip", "uv", "poetry"},
	model.ProtocolNPM:  {"npm", "pnpm", "yarn"},
}

// clientsFor lists the clients a protocol has commands for, default first.
func clientsFor(p model.Protocol) []string { return clientsByProtocol[p] }

// pickClient returns want when it is one of the protocol's clients, and the
// default otherwise.
func pickClient(p model.Protocol, want string) string {
	clients := clientsByProtocol[p]
	for _, c := range clients {
		if c == want {
			return c
		}
	}
	if len(clients) == 0 {
		return ""
	}
	return clients[0]
}

// safeWord is what a name, version, digest, or tag may look like to be put
// in a command unquoted. Registry validation already holds everything stored to
// this, so a value outside it means something is wrong, and the safe answer
// is no command at all rather than an escaped one.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9@+=:,./_-]+$`)

// safeHost is the same for the origin: letters, digits, dots, hyphens, a colon
// for the port, and brackets for an IPv6 literal.
var safeHost = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+$`)

func (in cmdInput) valid() bool {
	u, err := url.Parse(in.Base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !safeHost.MatchString(u.Host) || u.Path != "" {
		return false
	}
	for _, w := range []string{in.Repo, in.Package, in.Version, in.Digest, in.Tag} {
		if w != "" && !safeWord.MatchString(w) {
			return false
		}
	}
	return in.Repo != ""
}

// hostPort is the registry host as OCI clients and .npmrc write it: no scheme.
func (in cmdInput) hostPort() string {
	u, err := url.Parse(in.Base)
	if err != nil {
		return ""
	}
	return u.Host
}

func (in cmdInput) hostname() string {
	u, err := url.Parse(in.Base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (in cmdInput) plainHTTP() bool { return strings.HasPrefix(in.Base, "http://") }

// loopback reports an address that docker and friends treat as a trusted
// plain-http registry without being told.
func (in cmdInput) loopback() bool {
	switch in.hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ociRef is the repository, and the image under it when there is one, the way
// a client names it after the host. The adapter splits it back at the
// repository boundary (resolve.SplitOCIPath).
func (in cmdInput) ociRef() string {
	ref := in.hostPort() + "/" + in.Repo
	if in.Package != "" && in.Package != registry.OCIRootPackage {
		ref += "/" + in.Package
	}
	return ref
}

// pypiIndex is the simple index of the repository, without credentials.
func (in cmdInput) pypiIndex() string { return in.Base + "/pypi/" + in.Repo + "/simple/" }

// pypiIndexWithToken carries the token as the password of the URL, which is
// how pip and uv send Basic credentials. The shell expands $SINDRI_TOKEN, so
// the command must be pasted where double quotes expand.
func (in cmdInput) pypiIndexWithToken() string { return in.pypiIndexAs("token:$SINDRI_TOKEN") }

// pypiIndexAs is the simple index with userinfo in front of the host.
func (in cmdInput) pypiIndexAs(userinfo string) string {
	scheme, _, _ := strings.Cut(in.Base, "://")
	return scheme + "://" + userinfo + "@" + in.hostPort() + "/pypi/" + in.Repo + "/simple/"
}

// npmRegistry is the repository's registry URL, with a trailing slash.
func (in cmdInput) npmRegistry() string { return in.Base + "/npm/" + in.Repo + "/" }

// npmAuthKey is the key a token line in .npmrc is filed under: the registry
// URL without its scheme.
func (in cmdInput) npmAuthKey() string { return "//" + in.hostPort() + "/npm/" + in.Repo + "/" }

// PullCommands are the commands that pull one OCI artifact: by digest, and by
// tag when the page was reached through one. The tool is the chosen client.
func PullCommands(in cmdInput) []cmdStep {
	if !in.valid() || in.Protocol != model.ProtocolOCI || in.Digest == "" {
		return nil
	}
	in.Client = pickClient(in.Protocol, in.Client)
	refs := []cmdStep{{Text: in.pull(in.ociRef() + "@" + in.Digest)}}
	if in.Tag != "" {
		refs = append(refs, cmdStep{Caption: "Or by tag, which can move", Text: in.pull(in.ociRef() + ":" + in.Tag)})
	}
	return refs
}

// pull is one pull of ref with the chosen client.
func (in cmdInput) pull(ref string) string {
	switch in.Client {
	case "podman":
		if in.plainHTTP() {
			return "podman pull --tls-verify=false " + ref
		}
		return "podman pull " + ref
	case "crane":
		// crane writes the image to a file, since it has no local daemon.
		insecure := ""
		if in.plainHTTP() {
			insecure = "--insecure "
		}
		return "crane pull " + insecure + ref + " image.tar"
	default:
		return "docker pull " + ref
	}
}

// ociNotes are the cautions that go with a client's OCI commands.
func ociNotes(in cmdInput) []string {
	if in.Protocol != model.ProtocolOCI || !in.valid() || !in.plainHTTP() || in.loopback() {
		return nil
	}
	return []string{"This server speaks plain http. Docker refuses that unless " + in.hostPort() + " is listed under insecure-registries in its daemon configuration."}
}

// InstallCommands are the commands that install one package, or one version of
// it, with the chosen client. An OCI package is pulled by name, at the tag when
// one is given. Version may be empty for the newest the index offers.
func InstallCommands(in cmdInput) []cmdStep {
	if !in.valid() || in.Package == "" {
		return nil
	}
	in.Client = pickClient(in.Protocol, in.Client)
	switch in.Protocol {
	case model.ProtocolOCI:
		ref := in.ociRef()
		if in.Tag != "" {
			ref += ":" + in.Tag
		}
		return []cmdStep{{Text: in.pull(ref)}}
	case model.ProtocolPyPI:
		spec := in.Package
		if in.Version != "" {
			spec += "==" + in.Version
		}
		return in.pypiInstall(spec)
	case model.ProtocolNPM:
		spec := in.Package
		if in.Version != "" {
			spec += "@" + in.Version
		}
		return in.npmInstall(spec)
	}
	return nil
}

func (in cmdInput) pypiInstall(spec string) []cmdStep {
	switch in.Client {
	case "uv":
		return []cmdStep{{Text: `uv pip install --index-url "` + in.pypiIndexWithToken() + `" ` + spec}}
	case "poetry":
		return []cmdStep{
			{Caption: "Once per project", Text: `poetry source add --priority=primary sindri "` + in.pypiIndex() + `"`},
			{Text: `poetry config http-basic.sindri token "$SINDRI_TOKEN"`},
			{Text: "poetry add " + spec},
		}
	default:
		cmd := `pip install --index-url "` + in.pypiIndexWithToken() + `"`
		if in.plainHTTP() {
			cmd += " --trusted-host " + in.hostPort()
		}
		return []cmdStep{{Text: cmd + " " + spec}}
	}
}

func (in cmdInput) npmInstall(spec string) []cmdStep {
	switch in.Client {
	case "pnpm":
		return []cmdStep{{Text: "pnpm add " + spec + " --registry " + in.npmRegistry()}}
	case "yarn":
		return []cmdStep{{Caption: "Yarn 1. Yarn 4 has no --registry flag and reads .yarnrc.yml, shown on the repository page", Text: "yarn add " + spec + " --registry " + in.npmRegistry()}}
	default:
		return []cmdStep{{Text: "npm install " + spec + " --registry " + in.npmRegistry()}}
	}
}

// installNotes tell the reader what the install command leaves to their
// configuration: the server authenticates every request.
func installNotes(in cmdInput) []string {
	if !in.valid() {
		return nil
	}
	switch in.Protocol {
	case model.ProtocolNPM:
		return []string{"Every request authenticates. Put your token in ~/.npmrc as the repository page shows, or this install is refused."}
	case model.ProtocolPyPI:
		return []string{"The token travels as the password of the index URL, so $SINDRI_TOKEN must be set in your shell."}
	case model.ProtocolOCI:
		return append([]string{"Run the login on the repository page first."}, ociNotes(in)...)
	}
	return nil
}

// SetupCommands are what a person puts in place once so that a client talks to
// this repository: login for OCI, an index for PyPI, a registry for npm.
func SetupCommands(in cmdInput) []cmdStep {
	if !in.valid() {
		return nil
	}
	in.Client = pickClient(in.Protocol, in.Client)
	switch in.Protocol {
	case model.ProtocolOCI:
		return in.ociSetup()
	case model.ProtocolPyPI:
		return in.pypiSetup()
	case model.ProtocolNPM:
		return in.npmSetup()
	}
	return nil
}

func (in cmdInput) ociSetup() []cmdStep {
	host := in.hostPort()
	var login string
	switch in.Client {
	case "podman":
		flag := ""
		if in.plainHTTP() {
			flag = " --tls-verify=false"
		}
		login = `echo "$SINDRI_TOKEN" | podman login ` + host + " -u token --password-stdin" + flag
	case "crane":
		login = `echo "$SINDRI_TOKEN" | crane auth login ` + host + " -u token --password-stdin"
	default:
		login = `echo "$SINDRI_TOKEN" | docker login ` + host + " -u token --password-stdin"
	}
	return []cmdStep{
		{Caption: "Sign in once. The user name is ignored; the password is your token", Text: login},
		{Caption: "Then pull by tag", Text: in.pull(in.ociRef() + "/IMAGE:TAG")},
	}
}

func (in cmdInput) pypiSetup() []cmdStep {
	switch in.Client {
	case "uv":
		return []cmdStep{{
			Caption: "uv.toml, in the project or in ~/.config/uv/",
			Text:    "[[index]]\nurl = \"" + in.pypiIndexAs("token:YOUR_TOKEN") + "\"\ndefault = true",
		}}
	case "poetry":
		return []cmdStep{
			{Caption: "Once per project", Text: `poetry source add --priority=primary sindri "` + in.pypiIndex() + `"`},
			{Text: `poetry config http-basic.sindri token "$SINDRI_TOKEN"`},
		}
	default:
		conf := "[global]\nindex-url = " + in.pypiIndexAs("token:YOUR_TOKEN")
		if in.plainHTTP() {
			conf += "\ntrusted-host = " + in.hostPort()
		}
		return []cmdStep{{Caption: "pip.conf: ~/.config/pip/pip.conf, or pip.ini on Windows. Use one index, never --extra-index-url", Text: conf}}
	}
}

func (in cmdInput) npmSetup() []cmdStep {
	if in.Client == "yarn" {
		yml := "npmRegistryServer: \"" + in.npmRegistry() + "\"\n"
		if in.plainHTTP() {
			yml += "unsafeHttpWhitelist:\n  - " + in.hostname() + "\n"
		}
		yml += "npmAlwaysAuth: true\nnpmAuthToken: \"${SINDRI_TOKEN}\""
		return []cmdStep{{Caption: ".yarnrc.yml, for Yarn 4. Yarn 1 reads the .npmrc files shown under npm", Text: yml}}
	}
	caption := "~/.npmrc keeps the token out of the project"
	if in.Client == "pnpm" {
		caption = "~/.npmrc: pnpm refuses to expand ${SINDRI_TOKEN} from a project .npmrc"
	}
	return []cmdStep{
		{Caption: ".npmrc in the project", Text: "registry=" + in.npmRegistry()},
		{Caption: caption, Text: in.npmAuthKey() + ":_authToken=${SINDRI_TOKEN}"},
	}
}

// manageInput is what the management commands for one artifact need.
type manageInput struct {
	Base       string
	Repo       string
	Digest     string
	PromoteTo  string // a stage to promote into
	RevokeFrom string // a stage the artifact is approved in
	// Rescan includes the re-scan command, which exists only with scanning on.
	Rescan bool
}

// ManageCommands are the management API calls for a hosted artifact:
// promote, revoke, and re-scan. A stage left empty becomes a placeholder.
// Proxy content has none of the three, which the caller decides by passing no
// manageInput at all.
func ManageCommands(in manageInput) []cmdStep {
	probe := cmdInput{Base: in.Base, Repo: in.Repo, Digest: in.Digest}
	if !probe.valid() || in.Digest == "" {
		return nil
	}
	call := func(action string, body string) string {
		cmd := "curl -fsS -X POST " + in.Base + "/api/v1/subjects/" + in.Digest + "/" + action +
			` -H "Authorization: Bearer $SINDRI_TOKEN"`
		if body != "" {
			cmd += ` -H "Content-Type: application/json" -d ` + shellSingleQuote(body)
		}
		return cmd
	}
	change := func(stage string) string {
		if stage == "" {
			stage = "STAGE"
		}
		b, _ := json.Marshal(struct {
			Repository string `json:"repository"`
			Stage      string `json:"stage"`
			Reason     string `json:"reason"`
		}{in.Repo, stage, "from the web interface"})
		return string(b)
	}
	steps := []cmdStep{
		{Caption: "Promote into a stage, which runs its gates", Text: call("promote", change(in.PromoteTo))},
		{Caption: "Revoke an approval", Text: call("revoke", change(in.RevokeFrom))},
	}
	if in.Rescan {
		steps = append(steps, cmdStep{Caption: "Scan again with the current vulnerability database", Text: call("scan", "")})
	}
	return steps
}

// shellSingleQuote quotes s for a POSIX shell.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
