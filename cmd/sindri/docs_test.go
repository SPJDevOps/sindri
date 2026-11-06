package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/config"
)

// The docs pages show commands, and docs that drift from the product are worse
// than none. Every page in docs/ with a shell block names what runs its
// commands in a comment:
//
//	<!-- commands checked by: scripts/m8-quickstart-gate.sh -->
//	<!-- commands checked by: TestDocs_ConfigurationCommandsRun -->
//
// scripts/m10-docs-gate.sh reads those comments and runs every check they
// name. The ADRs are exempt: they are records of a decision, never rewritten,
// and their blocks quote what was true when it was made.

const docsDir = "../../docs"

var (
	shellFence   = regexp.MustCompile("(?m)^ *```(bash|sh|shell|console)\\s*$")
	checkedBy    = regexp.MustCompile(`<!-- commands checked by: (\S+) -->`)
	summaryEntry = regexp.MustCompile(`\]\(([^)]+\.md)\)`)
)

// docsPages lists every markdown page under docs/, relative to it.
func docsPages(t *testing.T) []string {
	t.Helper()
	var pages []string
	err := filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		rel, err := filepath.Rel(docsDir, path)
		pages = append(pages, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

func TestDocs_EveryPageWithCommandsNamesItsCheck(t *testing.T) {
	tests := testFuncs(t)
	for _, page := range docsPages(t) {
		if strings.HasPrefix(page, "adr/") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(docsDir, page))
		if err != nil {
			t.Fatal(err)
		}
		checks := checkedBy.FindAllSubmatch(body, -1)
		if shellFence.Match(body) && len(checks) == 0 {
			t.Errorf("docs/%s shows commands but names no check; add <!-- commands checked by: <script or test> -->", page)
		}
		for _, c := range checks {
			name := string(c[1])
			switch {
			case strings.HasPrefix(name, "scripts/"):
				info, err := os.Stat(filepath.Join("../..", name))
				if err != nil || info.Mode()&0o111 == 0 {
					t.Errorf("docs/%s names %s, which is not an executable script", page, name)
				}
			case strings.HasPrefix(name, "Test"):
				if !tests[name] {
					t.Errorf("docs/%s names %s, which is not a test in this repository", page, name)
				}
			default:
				t.Errorf("docs/%s names %s; a check is scripts/<file> or a Test function", page, name)
			}
		}
	}
}

// testFuncs lists the Test functions defined anywhere in the module.
func testFuncs(t *testing.T) map[string]bool {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	out := map[string]bool{}
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		for _, m := range decl.FindAllSubmatch(src, -1) {
			out[string(m[1])] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mdBook ignores a page SUMMARY.md does not list, so a new page would be in
// the repository and missing from the site with nothing to say so (ADR 26).
func TestDocs_EveryPageIsInTheSummary(t *testing.T) {
	summary, err := os.ReadFile(filepath.Join(docsDir, "SUMMARY.md"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, m := range summaryEntry.FindAllSubmatch(summary, -1) {
		listed[string(m[1])] = true
	}
	for _, page := range docsPages(t) {
		if page != "SUMMARY.md" && !listed[page] {
			t.Errorf("docs/%s is not in docs/SUMMARY.md, so the docs site leaves it out", page)
		}
	}
	for page := range listed {
		if _, err := os.Stat(filepath.Join(docsDir, page)); err != nil {
			t.Errorf("docs/SUMMARY.md lists %s, which does not exist", page)
		}
	}
}

// fencedBlocks returns the bodies of the page's code blocks in a language.
func fencedBlocks(t *testing.T, page, lang string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(docsDir, page))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile("(?ms)^```" + lang + "\\s*\n(.*?)^```")
	var out []string
	for _, m := range block.FindAllSubmatch(body, -1) {
		out = append(out, string(m[1]))
	}
	if len(out) == 0 {
		t.Fatalf("docs/%s has no %s block", page, lang)
	}
	return out
}

// TestDocs_ConfigurationCommandsRun runs the commands docs/configuration.md
// shows, as written, against the file that exercises every configuration
// field.
func TestDocs_ConfigurationCommandsRun(t *testing.T) {
	blocks := fencedBlocks(t, "configuration.md", "bash")
	golden, err := os.ReadFile("../../testdata/config/sindri.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sindri.yaml"), golden, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	ran := 0
	for _, block := range blocks {
		for line := range strings.Lines(block) {
			args := strings.Fields(line)
			if len(args) == 0 || strings.HasPrefix(args[0], "#") {
				continue
			}
			if args[0] != "sindri" {
				t.Fatalf("docs/configuration.md runs %q, which this test cannot; give it a check of its own", line)
			}
			t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
				cmd := newRootCmd()
				cmd.SetArgs(args[1:])
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				if err := cmd.ExecuteContext(t.Context()); err != nil {
					t.Fatalf("%s: %v\n%s", line, err, out.String())
				}
				if out.Len() == 0 {
					t.Errorf("%s printed nothing", line)
				}
			})
			ran++
		}
	}
	if ran == 0 {
		t.Fatal("docs/configuration.md shows no sindri command")
	}
}

// TestDocs_ConfigurationExamplesLoad loads every YAML example on the
// configuration page, so an example cannot use a key the loader refuses.
func TestDocs_ConfigurationExamplesLoad(t *testing.T) {
	envRef := regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)`)
	for i, block := range fencedBlocks(t, "configuration.md", "yaml") {
		for _, m := range envRef.FindAllStringSubmatch(block, -1) {
			t.Setenv(m[1], "example")
		}
		if _, err := config.Parse([]byte(block), "configuration.md"); err != nil {
			t.Errorf("example %d does not load: %v\n%s", i+1, err, block)
		}
	}
}
