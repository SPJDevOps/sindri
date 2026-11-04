//go:build differential

package npm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"
	"time"
)

// This test checks ADR 24's promise that the manifest Sindri reads from a tarball at
// publish is the manifest a client installs. Each tarball shape the parser accepts is
// installed from a local file by npm, pnpm and yarn, with scripts off, and the
// package.json that lands in node_modules is compared with the one inspectManifest
// returned. Each tarball holds only package.json variants and placeholder files, whose
// name, version and description differ, so a disagreement shows which one was read.
//
// Run it through scripts/npm-tarball-differential.sh, which pins the clients and keeps
// every file they write in a temp directory.

const installTimeout = 3 * time.Minute

// differentialClient installs one dependency, given as a file: spec in the project's
// package.json, into the current directory. Each client gets its own cache and store
// under cacheDir.
type differentialClient struct {
	name string
	env  string
	args func(cacheDir string) []string
}

var differentialClients = []differentialClient{
	{name: "npm", env: "SINDRI_DIFF_NPM", args: func(string) []string {
		return []string{"install", "--ignore-scripts", "--no-audit", "--no-fund", "--no-package-lock", "--offline"}
	}},
	{name: "pnpm", env: "SINDRI_DIFF_PNPM", args: func(cacheDir string) []string {
		return []string{"install", "--ignore-scripts", "--offline", "--store-dir", filepath.Join(cacheDir, "pnpm-store")}
	}},
	{name: "yarn", env: "SINDRI_DIFF_YARN", args: func(cacheDir string) []string {
		return []string{"install", "--ignore-scripts", "--no-lockfile", "--non-interactive", "--silent", "--cache-folder", filepath.Join(cacheDir, "yarn-cache")}
	}},
}

// shapeManifests holds what a shape's tarball says is its package.json, and the
// decoy, the other document a hostile shape tries to get installed in its place.
type shapeManifests struct {
	real, decoy string
	// decoyName is the name in decoy, which a client that installed the decoy may
	// place the package under.
	decoyName string
}

type differentialShape struct {
	name string
	// file is a committed tarball, used as it is.
	file string
	// refused is the outcome expected of the parser. A shape expected to be refused
	// is not sent to the clients, unless it is a control.
	refused bool
	// control sends a shape the parser refuses to the clients anyway. At least one
	// client must install the decoy, or the test cannot tell a disagreement from
	// nothing.
	control bool
	// build returns the entries of a tarball.
	build func(m shapeManifests) [][]byte
}

// tarStreamEntry writes a header the way tar-stream 3.2 does.
func tarStreamEntry(name string, size int, typeflag byte) []byte {
	b := make([]byte, 512)
	copy(b, name)
	copy(b[100:], "000644 \x00")
	copy(b[108:], "000000 \x00")
	copy(b[116:], "000000 \x00")
	copy(b[124:], fmt.Sprintf("%011o ", size))
	copy(b[136:], "15272107331 ")
	b[156] = typeflag
	copy(b[257:], ustarMagic)
	copy(b[329:], "000000 \x00")
	copy(b[337:], "000000 \x00")
	copy(b[148:], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o \x00", sum))
	return b
}

func differentialShapes() []differentialShape {
	overUstar := "package/" + strings.Repeat("d/", 150) + "f.js"
	manifest := func(m shapeManifests) []byte { return rawEntry(manifestEntry, m.real, '0') }
	decoy := func(m shapeManifests, name string) []byte { return rawEntry(name, m.decoy, '0') }
	// decoyEntry is a whole package/package.json tar entry holding the decoy. Placed
	// in the body of another entry, the parser skips it, but a client that misframes
	// that entry lands on it and installs the decoy.
	decoyEntry := func(m shapeManifests) string { return string(rawEntry(manifestEntry, m.decoy, '0')) }
	field := func(off int, value string) func([]byte) {
		return func(b []byte) { copy(b[off:], value) }
	}

	shapes := []differentialShape{
		// Accepted forms: what real tools write.
		{name: "npm pack, pax path", file: "npm-pack-pax-path.tgz"},
		{name: "npm pack, ustar prefix", file: "npm-pack-ustar-prefix.tgz"},
		{name: "tar-stream form", build: func(m shapeManifests) [][]byte {
			pax := paxRecords("path", overUstar)
			return [][]byte{
				append(tarStreamEntry(manifestEntry, len(m.real), '0'), padded(m.real)...),
				append(tarStreamEntry("PaxHeader", len(pax), 'x'), padded(pax)...),
				// Built to catch a reader that takes an entry named PaxHeader for
				// metadata and skips only its header, so it lands on the body.
				append(tarStreamEntry("PaxHeader", len(decoyEntry(m)), '0'), decoyEntry(m)...),
				tarStreamEntry("package/dir", 0, '5'),
			}
		}},
		{name: "npm form with no owner", build: func(m shapeManifests) [][]byte {
			return [][]byte{append(patchedHeader(manifestEntry, len(m.real), '0', func(b []byte) { clear(b[108:124]) }), padded(m.real)...)}
		}},
		{name: "NUL typeflag", build: func(m shapeManifests) [][]byte {
			return [][]byte{append(rawHeader(manifestEntry, len(m.real), 0, ustarMagic), padded(m.real)...)}
		}},
		{name: "directories with and without a slash", build: func(m shapeManifests) [][]byte {
			return [][]byte{rawHeader("package", 0, '5', ustarMagic), rawHeader("package/lib/", 0, '5', ustarMagic), manifest(m)}
		}},
		{name: "manifest after other files", build: func(m shapeManifests) [][]byte {
			return [][]byte{rawEntry("package/a.js", "x", '0'), rawHeader("package/lib/", 0, '5', ustarMagic), manifest(m)}
		}},
		{name: "extra zero blocks", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), make([]byte, 8192)}
		}},
		{name: "zero bytes short of a block", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), make([]byte, 1024+7)}
		}},

		// Accepted forms where a second parser could read a different manifest.
		{name: "manifest split over prefix and name", build: func(m shapeManifests) [][]byte {
			return [][]byte{prefixedEntry("package", "package.json", m.real)}
		}},
		{name: "manifest named by a pax path over a short name", build: func(m shapeManifests) [][]byte {
			return [][]byte{paxEntry(paxRecords("path", manifestEntry)), rawEntry("package/long-name-cut-short", m.real, '0')}
		}},
		{name: "pax with every allowed key", build: func(m shapeManifests) [][]byte {
			return [][]byte{
				manifest(m),
				paxEntry(paxRecords("path", "package/a.js", "mtime", "1.5", "atime", "1", "ctime", "1", "comment", "a=b")),
				rawEntry("package/a.js", "x", '0'),
			}
		}},
		{name: "pax with no path", build: func(m shapeManifests) [][]byte {
			return [][]byte{paxEntry(paxRecords("mtime", "1")), manifest(m)}
		}},
		{name: "pax size repeating the header's", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", overUstar, "mtime", "499162500", "size", fmt.Sprint(len(decoyEntry(m))))),
				// Built to catch a reader that ignores the pax size, or reads the entry as
				// empty, so it lands on the body.
				rawEntry(overUstar[:100], decoyEntry(m), '0')}
		}},
		{name: "pax path over a name ending in a slash", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "package/a.js")),
				// Built to catch a reader that takes the slash for a directory with no body.
				rawEntry("package/cut-short/", decoyEntry(m), '0')}
		}},
		{name: "pax path over a name cut after a parent segment", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "package/a.js")),
				// Built to catch a reader that ignores the pax path, sees a directory at
				// package, and so skips no body.
				rawEntry("package/cut/..", decoyEntry(m), '0')}
		}},
		{name: "pax path over a name cut inside a character", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "package/é.js")),
				// Built to catch a reader that drops an entry for its invalid name without
				// skipping its body.
				rawEntry("package/\xc3", decoyEntry(m), '0')}
		}},
		{name: "ustar prefix naming a nested package.json", build: func(m shapeManifests) [][]byte {
			// Built to catch a reader that ignores the prefix, which sees package.json
			// at the archive root.
			return [][]byte{manifest(m), prefixedEntry("package/a", "package.json", m.decoy)}
		}},
		{name: "decoy manifest as a plain second file", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/also-harmless.js")}
		}},
		{name: "decoy manifest one directory down", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/nested/package.json")}
		}},
		{name: "decoy manifest two directories down", build: func(m shapeManifests) [][]byte {
			return [][]byte{decoy(m, "package/a/b/package.json"), manifest(m)}
		}},
		{name: "decoy manifest under the NTFS short-name lookalike", build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/nested/PACKAG~1.JSO")}
		}},

		// Shapes the parser should refuse. They are compared only if it accepts them.
		{name: "refused: second manifest", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, manifestEntry)}
		}},
		{name: "refused: decoy under another top directory", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "other/package.json")}
		}},
		{name: "refused: decoy in upper case", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/PACKAGE.JSON")}
		}},
		{name: "refused: decoy under a backslash name", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, `package\package.json`)}
		}},
		{name: "refused: decoy under a dot segment", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/./package.json")}
		}},
		{name: "refused: decoy under a doubled slash", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package//package.json")}
		}},
		{name: "refused: decoy under a trailing dot", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/package.json.")}
		}},
		{name: "refused: decoy named by a pax path", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", manifestEntry)), rawEntry("package/harmless.js", m.decoy, '0')}
		}},
		{name: "refused: decoy named by a pax path with a dot segment", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "./package/package.json")), rawEntry("package/harmless.js", m.decoy, '0')}
		}},
		{name: "refused: decoy after a type N path", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), rawEntry("x", manifestEntry+"\x00", 'N'), rawEntry("package/harmless.js", m.decoy, '0')}
		}},
		{name: "refused: decoy after a GNU long name", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), rawEntry("././@LongLink", manifestEntry+"\x00", 'L'), rawEntry("package/harmless.js", m.decoy, '0')}
		}},
		{name: "refused: decoy behind a GNU magic header", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), append(rawHeader(manifestEntry, len(m.decoy), '0', "ustar  \x00"), padded(m.decoy)...)}
		}},
		{name: "refused: decoy behind a signed checksum", refused: true, build: func(m shapeManifests) [][]byte {
			h := rawHeader(manifestEntry, len(m.decoy), '0', ustarMagic)
			h[500] = 0xff
			copy(h[148:], "        ")
			sum := 0
			for _, c := range h {
				sum += int(int8(c))
			}
			copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
			return [][]byte{manifest(m), append(h, padded(m.decoy)...)}
		}},
		{name: "refused: decoy behind a linkname", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), append(patchedHeader(manifestEntry, len(m.decoy), '0', field(157, "x")), padded(m.decoy)...)}
		}},
		{name: "refused: symlink to the manifest", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), patchedHeader("package/l.json", 0, '2', field(157, "package.json"))}
		}},
		{name: "refused: decoy under a drive-letter name", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/c:/package.json")}
		}},
		{name: "refused: base-256 size", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), patchedHeader("package/z.js", 0, '0', field(124, "\x80\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))}
		}},
		{name: "refused: one zero block before more data", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), make([]byte, 512), decoy(m, manifestEntry)}
		}},
		{name: "refused: pax path over the manifest's own name", refused: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "package/a.js")), decoy(m, manifestEntry)}
		}},
	}

	// Names a pax path replaces that only look like the manifest, which is the
	// riskiest class the parser accepts. Each carries the decoy.
	for _, name := range []string{
		"PaxHeader", "package/cut-short/", "package/cut-short.", "package/cut-short ", "package/c:",
		"package/foo:bar.js", "package/nested/package.json", "package/nested/c:/package.json",
		"package/c:/nested/package.json", "//server/share/package/nested/package.json",
		"//?/c:/package/nested/package.json", "ab:/package/nested/package.json", "package/1:/package.json",
		"package/package.jso", "package/package.json.js", "package/nested/PACKAG~1.JSO",
		"package/PACKAG~1.JSON", "package/README~1.JSO",
	} {
		shapes = append(shapes, differentialShape{name: "pax path replacing " + name, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), paxEntry(paxRecords("path", "package/a.js")), rawEntry(name, m.decoy, '0')}
		}})
	}

	// Controls: shapes the parser refuses because an installer would read the decoy.
	// If no client installs it, the comparison could not have shown a disagreement.
	return append(shapes,
		differentialShape{name: "control: second manifest", refused: true, control: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, manifestEntry)}
		}},
		differentialShape{name: "control: manifest under another top directory", refused: true, control: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "other/package.json")}
		}},
		differentialShape{name: "control: manifest in upper case", refused: true, control: true, build: func(m shapeManifests) [][]byte {
			return [][]byte{manifest(m), decoy(m, "package/PACKAGE.JSON")}
		}},
	)
}

type fieldsRead struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

func (f fieldsRead) String() string {
	return fmt.Sprintf("%s@%s %s", f.Name, f.Version, f.Description)
}

// allowedClientErrors names the client errors that do not fail the test, keyed by
// shape and client. A client error is otherwise a failure: it means nothing was
// compared.
var allowedClientErrors = map[string]string{
	"zero bytes short of a block|yarn": "SIN-59",
}

type differentialRow struct {
	shape, client, verdict, sindri, installed string
}

func TestDifferential_TarballParserAgreesWithInstallers(t *testing.T) {
	workDir := os.Getenv("SINDRI_DIFF_WORK")
	if workDir == "" {
		t.Fatal("SINDRI_DIFF_WORK is not set: run scripts/npm-tarball-differential.sh")
	}
	paths := make(map[string]string)
	for _, c := range differentialClients {
		p := os.Getenv(c.env)
		if p == "" {
			t.Fatalf("%s is not set: run scripts/npm-tarball-differential.sh", c.env)
		}
		paths[c.name] = p
	}

	shapes := differentialShapes()
	var (
		rows    = make([][]differentialRow, len(shapes))
		refused = make([]string, len(shapes))
	)
	t.Run("shapes", func(t *testing.T) {
		for i, shape := range shapes {
			t.Run(shape.name, func(t *testing.T) {
				t.Parallel()
				m := shapeManifests{
					real:      fmt.Sprintf(`{"name":"diff-shape-%02d","version":"1.%d.0","description":"READ-BY-SINDRI"}`, i, i),
					decoy:     fmt.Sprintf(`{"name":"decoy-shape-%02d","version":"9.9.%d","description":"INSTALLED-VARIANT"}`, i, i),
					decoyName: fmt.Sprintf("decoy-shape-%02d", i),
				}
				var data []byte
				if shape.file != "" {
					var err error
					if data, err = os.ReadFile(filepath.Join("testdata", shape.file)); err != nil {
						t.Fatal(err)
					}
				} else {
					data = rawTarball(t, shape.build(m)...)
				}

				read, err := inspectManifest(bytes.NewReader(data))
				switch {
				case err != nil && !errors.Is(err, errTarballRefused):
					t.Fatalf("inspectManifest failed for a reason other than a refusal: %v", err)
				case err != nil && !shape.refused:
					t.Errorf("inspectManifest refused a shape expected to be accepted: %v", err)
				case err == nil && shape.refused:
					t.Errorf("inspectManifest accepted a shape expected to be refused: %s", read)
				}
				if err != nil {
					refused[i] = err.Error()
					if !shape.control {
						return
					}
				}

				// A control is compared with the first manifest, which is what the
				// parser would have kept had it not refused.
				var sindri fieldsRead
				source := read
				if err != nil {
					source = json.RawMessage(m.real)
				}
				if err := json.Unmarshal(source, &sindri); err != nil {
					t.Fatalf("parser result is not JSON: %v", err)
				}

				shapeDir := filepath.Join(workDir, fmt.Sprintf("shape-%02d", i))
				tarballPath := filepath.Join(shapeDir, "tarball", "shape.tgz")
				if err := os.MkdirAll(filepath.Dir(tarballPath), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tarballPath, data, 0o600); err != nil {
					t.Fatal(err)
				}

				for _, c := range differentialClients {
					row := runClient(t, c, paths[c.name], shapeDir, tarballPath, sindri, shape.name, m.decoyName)
					row.shape = shape.name
					rows[i] = append(rows[i], row)
				}
			})
		}
	})

	var (
		out, controlOut                                strings.Builder
		compared, skipped, agreed, disagreed, failures int
		controlDisagreed                               int
		skippedNames                                   []string
	)
	header := "SHAPE\tCLIENT\tRESULT\tSINDRI READ\tCLIENT INSTALLED\n"
	tw := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	ctw := tabwriter.NewWriter(&controlOut, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, header)
	fmt.Fprint(ctw, header)
	for i, shape := range shapes {
		if shape.control {
			for _, row := range rows[i] {
				fmt.Fprintf(ctw, "%s\t%s\t%s\t%s\t%s\n", row.shape, row.client, row.verdict, row.sindri, row.installed)
				if row.verdict == "DISAGREE" {
					controlDisagreed++
				}
			}
			continue
		}
		if refused[i] != "" {
			skipped++
			skippedNames = append(skippedNames, shape.name+": "+refused[i])
			continue
		}
		compared++
		for _, row := range rows[i] {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", row.shape, row.client, row.verdict, row.sindri, row.installed)
			switch {
			case row.verdict == "agree":
				agreed++
			case row.verdict == "DISAGREE":
				disagreed++
				t.Errorf("%s: %s installed a different package.json from the one the parser read", row.shape, row.client)
			default:
				failures++
				if !strings.HasPrefix(row.verdict, "client error (allowed") {
					t.Errorf("%s: %s: %s", row.shape, row.client, row.installed)
				}
			}
		}
	}
	_ = tw.Flush()
	_ = ctw.Flush()

	fmt.Println(out.String())
	fmt.Printf("Shapes: %d candidates (excluding controls), %d compared, %d refused by the parser and so skipped.\n", compared+skipped, compared, skipped)
	fmt.Printf("Rows: %d agree, %d DISAGREE, %d client errors (the client refused or failed to install).\n", agreed, disagreed, failures)
	fmt.Println("Refused shapes:")
	for _, line := range skippedNames {
		fmt.Printf("  %s\n", line)
	}
	fmt.Println()
	fmt.Println("Negative controls: shapes the parser refuses, sent to the clients anyway. Not counted above.")
	fmt.Println(controlOut.String())
	fmt.Println("Not tested: Windows name handling (drive roots, 8.3 short names, trailing dots and spaces,")
	fmt.Println("UNC and device roots, backslashes) cannot be exercised here; the parser's refusals for them")
	fmt.Println("rest on reasoning and on Unix reproductions only.")

	if compared == 0 {
		t.Error("no shape was compared")
	}
	if controlDisagreed == 0 {
		t.Error("no negative control showed a client installing the decoy, so the comparison could not have caught a disagreement")
	}
}

// runClient installs the tarball with one client in a fresh project and reports
// what landed in node_modules. A package the client placed under the decoy's name
// is a disagreement, not a missing install.
func runClient(t *testing.T, c differentialClient, bin, shapeDir, tarballPath string, sindri fieldsRead, shapeName, decoyName string) differentialRow {
	t.Helper()
	row := differentialRow{client: c.name, sindri: sindri.String()}

	project := filepath.Join(shapeDir, c.name, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	host, err := json.Marshal(map[string]any{
		"name":         "differential-host",
		"version":      "0.0.0",
		"private":      true,
		"dependencies": map[string]string{sindri.Name: "file:" + tarballPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), host, 0o600); err != nil {
		t.Fatal(err)
	}

	clientError := func(detail string) differentialRow {
		row.verdict = "client error"
		if reason, ok := allowedClientErrors[shapeName+"|"+c.name]; ok {
			row.verdict = "client error (allowed: " + reason + ")"
		}
		row.installed = oneLine(detail)
		return row
	}

	ctx, cancel := context.WithTimeout(t.Context(), installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, c.args(filepath.Join(shapeDir, c.name))...) //nolint:gosec // G204: a client path chosen by the script that runs this test
	cmd.Dir = project
	if output, err := cmd.CombinedOutput(); err != nil {
		return clientError(fmt.Sprintf("%v: %s", err, output))
	}

	read := func(name string) (fieldsRead, error) {
		var f fieldsRead
		raw, err := os.ReadFile(filepath.Join(project, "node_modules", filepath.FromSlash(name), "package.json"))
		if err != nil {
			return f, err
		}
		return f, json.Unmarshal(raw, &f)
	}
	installed, err := read(sindri.Name)
	if errors.Is(err, os.ErrNotExist) && decoyName != "" {
		if decoy, derr := read(decoyName); derr == nil {
			row.verdict, row.installed = "DISAGREE", decoy.String()
			return row
		}
	}
	if err != nil {
		return clientError("installed no package.json: " + err.Error())
	}
	row.installed = installed.String()
	row.verdict = "agree"
	if installed != sindri {
		row.verdict = "DISAGREE"
	}
	return row
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
