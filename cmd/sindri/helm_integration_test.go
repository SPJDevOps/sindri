//go:build integration

package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/testsupport"
)

// helmRepos declares the hosted OCI repository the charts are pushed into.
const helmRepos = "repos:\n  charts: { protocol: oci }\n"

// helm runs the real helm binary against one serve. Its registry login, cache,
// and configuration live in a directory of the test's own, so the test never
// reads or changes the developer's helm setup.
type helm struct {
	t    *testing.T
	bin  string
	home string
}

// newHelm finds helm on PATH. A developer without it skips; CI installs a
// pinned helm, so there a missing binary fails rather than skipping quietly.
func newHelm(t *testing.T) *helm {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("helm is not on PATH; CI must install it: %v", err)
		}
		t.Skip("helm is not on PATH")
	}
	return &helm{t: t, bin: bin, home: t.TempDir()}
}

// run runs helm with args and stdin, returning its combined output.
func (h *helm) run(stdin string, args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.CommandContext(h.t.Context(), h.bin, args...) //nolint:gosec // G204: the test's own helm binary and arguments
	cmd.Env = append(os.Environ(),
		"HELM_REGISTRY_CONFIG="+filepath.Join(h.home, "registry.json"),
		"HELM_CACHE_HOME="+filepath.Join(h.home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(h.home, "config"),
		"HELM_DATA_HOME="+filepath.Join(h.home, "data"),
	)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *helm) mustRun(stdin string, args ...string) string {
	h.t.Helper()
	out, err := h.run(stdin, args...)
	if err != nil {
		h.t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// writeChart writes a minimal chart whose one template renders a value, so
// `helm template` proves the pulled chart is the one that was pushed.
func writeChart(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: demo\nversion: 0.1.0\n",
		"values.yaml": "greeting: hello from sindri\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n" +
			"data:\n  greeting: {{ .Values.greeting | quote }}\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHelm_ChartRoundTripsThroughHostedOCI is the README's "Helm charts" claim,
// end to end with the real client: package a chart, push it to a hosted OCI
// repository, pull it back byte-identical, and render it straight from the
// registry. Helm stores charts as OCI artifacts with their own media types, so
// this is the check that Sindri does not assume every manifest is an image.
func TestHelm_ChartRoundTripsThroughHostedOCI(t *testing.T) {
	h := newHelm(t)
	s := startServe(t, testsupport.DatabaseURL(t), helmRepos, nil)
	host := strings.TrimPrefix(s.base, "http://")
	ref := "oci://" + host + "/charts/demo"
	work := t.TempDir()

	src := filepath.Join(work, "src")
	writeChart(t, src)
	h.mustRun("", "package", src, "--destination", work)
	packaged := filepath.Join(work, "demo-0.1.0.tgz")
	want, err := os.ReadFile(packaged) //nolint:gosec // G304: a file this test just wrote
	if err != nil {
		t.Fatal(err)
	}

	// There is no anonymous access (ADR 18), so before logging in even a
	// pull is refused.
	if out, err := h.run("", "pull", ref, "--version", "0.1.0", "--plain-http", "--destination", t.TempDir()); err == nil {
		t.Fatalf("helm pull without logging in succeeded:\n%s", out)
	}

	h.mustRun(bootstrapToken, "registry", "login", host, "--username", "ci", "--password-stdin", "--plain-http")
	if out := h.mustRun("", "push", packaged, "oci://"+host+"/charts", "--plain-http"); !strings.Contains(out, "sha256:") {
		t.Fatalf("helm push printed no digest:\n%s", out)
	}

	resp := s.request(http.MethodGet, "/api/v1/repositories/charts/packages", nil, bearer(bootstrapToken))
	if resp.status != http.StatusOK || !bytes.Contains(resp.body, []byte(`"demo"`)) {
		t.Fatalf("charts packages after push: %d %s", resp.status, resp.body)
	}

	pulled := t.TempDir()
	h.mustRun("", "pull", ref, "--version", "0.1.0", "--plain-http", "--destination", pulled)
	got, err := os.ReadFile(filepath.Join(pulled, "demo-0.1.0.tgz")) //nolint:gosec // G304: the file helm pull was told to write
	if err != nil {
		t.Fatalf("helm pull wrote no chart archive: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("pulled chart differs from the pushed one: %d bytes, want %d", len(got), len(want))
	}

	out := h.mustRun("", "template", "rt", ref, "--version", "0.1.0", "--plain-http")
	for _, line := range []string{"kind: ConfigMap", "name: rt", `greeting: "hello from sindri"`} {
		if !strings.Contains(out, line) {
			t.Fatalf("helm template from the registry lacks %q:\n%s", line, out)
		}
	}
}
