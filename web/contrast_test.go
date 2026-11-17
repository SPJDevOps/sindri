package web_test

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spj/sindri/web"
)

// minContrast is WCAG AA for normal text.
const minContrast = 4.5

// colourTokens reads every --name: #rrggbb custom property out of the
// :root block for the light theme and out of the prefers-color-scheme: dark
// block for the dark one. The dark theme inherits what it does not set.
func colourTokens(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	css, err := fs.ReadFile(web.Static(), "sindri.css")
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(--[a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;`)
	read := func(block string) map[string]string {
		out := map[string]string{}
		for _, m := range decl.FindAllStringSubmatch(block, -1) {
			out[m[1]] = strings.ToLower(m[2])
		}
		return out
	}
	l := regexp.MustCompile(`(?s):root\s*\{([^}]*)\}`).FindStringSubmatch(string(css))
	d := regexp.MustCompile(`(?s)prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{([^}]*)\}`).FindStringSubmatch(string(css))
	if l == nil || d == nil {
		t.Fatal("sindri.css: no :root block, or no dark :root block")
	}
	light = read(l[1])
	dark = read(d[1])
	for k, v := range light {
		if _, ok := dark[k]; !ok {
			dark[k] = v
		}
	}
	return light, dark
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	var ch [3]float64
	for i := range ch {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("colour %q: %v", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func ratio(t *testing.T, fg, bg string) float64 {
	t.Helper()
	a, b := luminance(t, fg), luminance(t, bg)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// Every foreground and background pair that DESIGN.md lists must reach
// 4.5:1 in both themes, and the numbers in the table must be the ones
// computed from sindri.css, so the table cannot go stale.
func TestDesignContrast_TableMatchesTokensAndPasses(t *testing.T) {
	light, dark := colourTokens(t)
	doc, err := os.ReadFile("DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `(--[a-z0-9-]+)` on `(--[a-z0-9-]+)` \\| ([0-9]+\\.[0-9]) \\| ([0-9]+\\.[0-9]) \\|\\s*$")
	rows := row.FindAllStringSubmatch(string(doc), -1)
	if len(rows) == 0 {
		t.Fatal("DESIGN.md lists no contrast pairs")
	}
	foreground := map[string]bool{}
	for _, r := range rows {
		fg, bg := r[1], r[2]
		foreground[fg] = true
		for _, theme := range []struct {
			name   string
			tokens map[string]string
			listed string
		}{{"light", light, r[3]}, {"dark", dark, r[4]}} {
			f, fok := theme.tokens[fg]
			b, bok := theme.tokens[bg]
			if !fok || !bok {
				t.Errorf("%s on %s: a token is not defined in sindri.css", fg, bg)
				continue
			}
			got := ratio(t, f, b)
			if got < minContrast {
				t.Errorf("%s on %s in %s: contrast %.2f is below %.1f", fg, bg, theme.name, got, minContrast)
			}
			if want := fmt.Sprintf("%.1f", got); want != theme.listed {
				t.Errorf("%s on %s in %s: DESIGN.md says %s, sindri.css gives %s", fg, bg, theme.name, theme.listed, want)
			}
		}
	}
	// Every colour that carries text must be in the table at least once.
	for _, token := range []string{
		"--text", "--text-muted", "--text-faint", "--accent", "--accent-hover", "--upstream", "--approved",
		"--revoked", "--sev-critical", "--sev-high", "--sev-medium", "--sev-low", "--sev-unknown", "--new",
	} {
		if !foreground[token] {
			t.Errorf("%s carries text but DESIGN.md lists no contrast pair for it", token)
		}
	}
}

// Every colour token is documented, so none gains a meaning nobody wrote down.
func TestDesignDoc_NamesEveryColourToken(t *testing.T) {
	light, _ := colourTokens(t)
	doc, err := os.ReadFile("DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	for token := range light {
		if !strings.Contains(string(doc), "`"+token+"`") {
			t.Errorf("DESIGN.md does not describe %s", token)
		}
	}
}

// The stylesheet stays one small file with no component nobody uses.
func TestStylesheet_IsSmallAndEveryClassIsUsed(t *testing.T) {
	css, err := fs.ReadFile(web.Static(), "sindri.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(css) >= 30*1024 {
		t.Errorf("sindri.css is %d bytes, the budget is 30 KB", len(css))
	}

	used := map[string]bool{}
	attr := regexp.MustCompile(`class="([^"]*)"`)
	tmplExpr := regexp.MustCompile(`\{\{[^}]*\}\}`)
	err = fs.WalkDir(web.Templates(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.Templates(), path)
		if err != nil {
			return err
		}
		for _, m := range attr.FindAllStringSubmatch(string(b), -1) {
			for _, c := range strings.Fields(tmplExpr.ReplaceAllString(m[1], " ")) {
				used[c] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Set on <html> by the script a later ticket adds.
	used["js-enhanced"] = true
	// Set by htmx on the element that is fetching.
	used["htmx-request"] = true

	bare := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`([^{};]+)\{`).FindAllStringSubmatch(bare, -1) {
		prelude := strings.TrimSpace(m[1])
		if strings.HasPrefix(prelude, "@") {
			continue
		}
		for _, c := range regexp.MustCompile(`\.([A-Za-z][\w-]*)`).FindAllStringSubmatch(prelude, -1) {
			if !used[c[1]] && !seen[c[1]] {
				seen[c[1]] = true
				t.Errorf("sindri.css styles .%s, which no template uses", c[1])
			}
		}
	}
}
