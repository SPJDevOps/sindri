package config

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The server settings tables of docs/configuration.md are generated from the
// structs in this package. Each section's block sits between
//
//	<!-- BEGIN GENERATED: <key> -->  and  <!-- END GENERATED: <key> -->
//
// and is rebuilt from the struct's yaml tags, each field's first comment
// paragraph, Default(), and each type's applyDefaults. The prose around the
// blocks stays hand-written. `make reference` rewrites the blocks.
var update = flag.Bool("update", false, "rewrite the generated blocks of docs/configuration.md")

const referencePage = "../../docs/configuration.md"

// shortForm is one documented scalar spelling of a section and the mapping it
// means. TestReference_ShortFormsDesugarAsDocumented loads both.
type shortForm struct {
	short string
	long  string
}

// shortForms lists every section that accepts a scalar, by its yaml key.
// TestReference_EveryScalarSectionDocumentsItsShortForm fails when a section
// learns a scalar spelling that is not listed here.
var shortForms = map[string][]shortForm{
	"listen":   {{short: `:8080`, long: `{address: ":8080"}`}},
	"storage":  {{short: `./data`, long: `{backend: filesystem, path: ./data}`}, {short: `s3://bucket/prefix`, long: `{backend: s3, bucket: bucket, prefix: prefix}`}},
	"database": {{short: `postgres://sindri.example.com/sindri`, long: `{url: "postgres://sindri.example.com/sindri"}`}},
	"log":      {{short: `debug`, long: `{level: debug}`}},
	"auth":     {{short: `${SINDRI_TOKEN}`, long: `{static_token: "${SINDRI_TOKEN}"}`}},
	"worker":   {{short: `false`, long: `{enabled: false}`}, {short: `true`, long: `{enabled: true}`}},
}

func TestReference_PageMatchesStructs(t *testing.T) {
	page, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderReference(page)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(referencePage, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, page) {
		t.Fatalf("docs/configuration.md is out of step with internal/config; run `make reference` and review the diff\n%s",
			firstDifference(page, got))
	}
}

func TestReference_EveryFieldIsDocumented(t *testing.T) {
	docs := fieldDocs(t)
	for _, sec := range serverSections() {
		for _, r := range sectionRows(sec, docs) {
			if r.meaning == "" {
				t.Errorf("%s: no comment on %s, so the reference has no meaning for it", r.key, r.goField)
			}
		}
	}
}

// statedDefaultChecks names, for every default a comment states, what keeps
// it true. A new "Default:" sentence fails
// TestReference_StatedDefaultsAreChecked until it is listed here.
var statedDefaultChecks = map[string]func(t *testing.T){
	"Storage.Region": func(t *testing.T) {
		c, err := Parse([]byte("storage: s3://bucket\n"), "s3.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.Region != "us-east-1" {
			t.Errorf("an S3 storage section without region gets %q, the page says us-east-1", c.Storage.Region)
		}
	},
	"Storage.Endpoint": func(t *testing.T) {
		c, err := Parse([]byte("storage: s3://bucket\n"), "s3.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.Endpoint != "" {
			t.Errorf("an S3 storage section without endpoint gets %q, the page says unset", c.Storage.Endpoint)
		}
	},
	// Applied by internal/auth when the browser login starts; checked there by
	// TestSession_LoginRequestsOpenIDProfileAndEmailByDefault.
	"OIDC.Scopes": func(*testing.T) {},
}

func TestReference_StatedDefaultsAreChecked(t *testing.T) {
	docs := fieldDocs(t)
	for _, sec := range serverSections() {
		for _, r := range sectionRows(sec, docs) {
			if !r.stated {
				continue
			}
			check, ok := statedDefaultChecks[r.goField]
			if !ok {
				t.Errorf("%s states its default in a comment (%s), and statedDefaultChecks has no check for it", r.goField, r.def)
				continue
			}
			t.Run(r.goField, check)
		}
	}
}

// The page's defaults come from Default(), and a loaded file gets its
// defaults from each section's applyDefaults. The two must agree, or the page
// describes a server with no file and not one whose file leaves a key out.
func TestReference_DefaultMatchesWhatEmptySectionsLoadTo(t *testing.T) {
	var doc strings.Builder
	for _, sec := range serverSections() {
		if sec.typ.Kind() == reflect.Struct {
			doc.WriteString(sec.key + ": {}\n")
		}
	}
	got, err := Parse([]byte(doc.String()), "empty-sections.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := Default(); !reflect.DeepEqual(*got, want) {
		t.Errorf("empty sections load to\n %+v\nbut Default() is\n %+v", *got, want)
	}
}

func TestReference_ShortFormsDesugarAsDocumented(t *testing.T) {
	t.Setenv("SINDRI_TOKEN", "a-token")
	for section, forms := range shortForms {
		for _, f := range forms {
			t.Run(section+"="+f.short, func(t *testing.T) {
				short, err := Parse([]byte(section+": "+f.short+"\n"), "short.yaml")
				if err != nil {
					t.Fatalf("short form: %v", err)
				}
				long, err := Parse([]byte(section+": "+f.long+"\n"), "long.yaml")
				if err != nil {
					t.Fatalf("long form: %v", err)
				}
				if !reflect.DeepEqual(short, long) {
					t.Errorf("%s: %s and %s load differently:\n short %+v\n long  %+v", section, f.short, f.long, short, long)
				}
			})
		}
	}
}

func TestReference_EveryScalarSectionDocumentsItsShortForm(t *testing.T) {
	sections := map[string]bool{}
	for _, sec := range serverSections() {
		sections[sec.key] = true
		if sec.typ.Kind() != reflect.Struct {
			continue
		}
		accepts := false
		for _, probe := range []string{"probe", "true"} {
			v := reflect.New(sec.typ)
			if yaml.Unmarshal([]byte(probe), v.Interface()) == nil {
				accepts = true
			}
		}
		if accepts && len(shortForms[sec.key]) == 0 {
			t.Errorf("%s accepts a scalar, but shortForms documents no short form for it", sec.key)
		}
		if !accepts && len(shortForms[sec.key]) > 0 {
			t.Errorf("shortForms documents a short form for %s, which accepts no scalar", sec.key)
		}
	}
	for key := range shortForms {
		if !sections[key] {
			t.Errorf("shortForms names %s, which is not a section", key)
		}
	}
}

// section is one top-level key of sindri.yaml with a struct field behind it.
type section struct {
	key   string
	field reflect.StructField
	typ   reflect.Type
	value reflect.Value // its default
}

// serverSections lists Config's yaml keys in declaration order, skipping the
// desired state, which the reference documents by hand.
func serverSections() []section {
	def := Default()
	rv := reflect.ValueOf(def)
	rt := rv.Type()
	var out []section
	for i := range rt.NumField() {
		f := rt.Field(i)
		key := yamlKey(f)
		if key == "" {
			continue
		}
		out = append(out, section{key: key, field: f, typ: f.Type, value: rv.Field(i)})
	}
	return out
}

func yamlKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" || !f.IsExported() {
		return ""
	}
	if name == "" {
		return strings.ToLower(f.Name)
	}
	return name
}

// row is one documented key.
type row struct {
	key     string // relative to the section, "" for a scalar section
	goField string // Type.Field, for messages
	typ     string
	meaning string
	def     string
	stated  bool   // the default came from a "Default:" sentence, not the code
	problem string // why this row cannot be generated; renderReference refuses it
}

var durationType = reflect.TypeOf(Duration(0))

// sectionRows flattens a section into one row per leaf key. A nested mapping
// contributes its keys under a dotted prefix; a map of name to mapping uses
// `<name>.` as the prefix.
func sectionRows(sec section, docs map[string]string) []row {
	if sec.typ.Kind() != reflect.Struct && sec.typ.Kind() != reflect.Map {
		return []row{leafRow("", "Config."+sec.field.Name, sec.typ, sec.value, docs)}
	}
	return structRows("", sec.typ, sec.value, docs)
}

func structRows(prefix string, t reflect.Type, v reflect.Value, docs map[string]string) []row {
	switch t.Kind() {
	case reflect.Pointer:
		elem := reflect.New(t.Elem())
		if !v.IsNil() {
			elem.Elem().Set(v.Elem())
		} else if d, ok := elem.Interface().(interface{ applyDefaults() }); ok {
			// An absent optional section has no default in Default(); the
			// defaults it gets once written are its type's.
			d.applyDefaults()
		}
		return structRows(prefix, t.Elem(), elem.Elem(), docs)
	case reflect.Map:
		if t.Elem().Kind() != reflect.Struct {
			return []row{{key: prefix, problem: "a map of " + t.Elem().Kind().String() + " is not supported by the generator"}}
		}
		return structRows(prefix+"<name>.", t.Elem(), reflect.New(t.Elem()).Elem(), docs)
	}

	var out []row
	for i := range t.NumField() {
		f := t.Field(i)
		key := yamlKey(f)
		if key == "" {
			continue
		}
		fv := v.Field(i)
		goField := t.Name() + "." + f.Name
		if strings.Contains(f.Tag.Get("yaml"), ",inline") {
			out = append(out, row{key: prefix + key, goField: goField, problem: "an inline field is not supported by the generator"})
			continue
		}
		switch {
		case f.Type == durationType:
			out = append(out, leafRow(prefix+key, goField, f.Type, fv, docs))
		case f.Type.Kind() == reflect.Struct,
			f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct,
			f.Type.Kind() == reflect.Map:
			out = append(out, structRows(prefix+key+".", f.Type, fv, docs)...)
		default:
			out = append(out, leafRow(prefix+key, goField, f.Type, fv, docs))
		}
	}
	return out
}

// statedDefault is a comment's last sentence, "Default: <value>.", for a
// default the code applies conditionally or outside this package.
var statedDefault = regexp.MustCompile(`(?:^|\.\s+)Default: (.+?)\.?$`)

func leafRow(key, goField string, t reflect.Type, v reflect.Value, docs map[string]string) row {
	r := row{key: key, goField: goField, typ: typeName(t), meaning: docs[goField], def: formatDefault(t, v)}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int32, reflect.Int64:
	case reflect.Slice:
		if t.Elem().Kind() != reflect.String {
			r.problem = "a list of " + t.Elem().Kind().String() + " is not supported by the generator"
		}
	default:
		r.problem = "a field of kind " + t.Kind().String() + " is not supported by the generator"
	}
	if m := statedDefault.FindStringSubmatchIndex(r.meaning); m != nil {
		stated := r.meaning[m[2]:m[3]]
		r.meaning = r.meaning[:m[0]]
		if r.meaning != "" && !strings.HasSuffix(r.meaning, ".") {
			r.meaning += "."
		}
		switch {
		case strings.Contains(stated, ". "):
			r.problem = "its \"Default:\" sentence must be the comment's last"
		case r.def != "unset":
			// Two sources for one default would drift; the code wins.
			r.problem = fmt.Sprintf("the code defaults to %s and its comment says %s; drop the comment's", r.def, stated)
		default:
			r.def, r.stated = stated, true
		}
	}
	return r
}

func typeName(t reflect.Type) string {
	if t == durationType {
		return "duration"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Slice:
		return "list of " + typeName(t.Elem()) + "s"
	default:
		return t.Kind().String()
	}
}

func formatDefault(t reflect.Type, v reflect.Value) string {
	if t == durationType {
		d := time.Duration(v.Int())
		if d == 0 {
			return "unset"
		}
		// time.Duration prints 24h as 24h0m0s; the page uses what an
		// operator would write.
		s := d.String()
		if strings.HasSuffix(s, "m0s") {
			s = strings.TrimSuffix(s, "0s")
		}
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
		return "`" + s + "`"
	}
	switch t.Kind() {
	case reflect.String:
		if v.String() == "" {
			return "unset"
		}
		return "`" + v.String() + "`"
	case reflect.Bool:
		return fmt.Sprintf("`%t`", v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("`%d`", v.Int())
	case reflect.Slice:
		if v.Len() == 0 {
			return "unset"
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = "`" + fmt.Sprint(v.Index(i).Interface()) + "`"
		}
		return strings.Join(parts, ", ")
	default:
		return "unset"
	}
}

// fieldDocs maps "Type.Field" to the first paragraph of the field's comment,
// on one line, read from this package's source.
func fieldDocs(t *testing.T) map[string]string {
	t.Helper()
	docs, err := readFieldDocs(".")
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func readFieldDocs(dir string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	docs := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				first, _, _ := strings.Cut(field.Doc.Text(), "\n\n")
				first = strings.Join(strings.Fields(first), " ")
				for _, id := range field.Names {
					docs[ts.Name.Name+"."+id.Name] = first
				}
			}
			return false
		})
	}
	return docs, nil
}

var generatedBlock = regexp.MustCompile(`(?s)<!-- BEGIN GENERATED: ([A-Za-z]+) -->\n.*?<!-- END GENERATED: ([A-Za-z]+) -->`)

// renderReference rebuilds every generated block of the page. Every server
// section must have exactly one block, and every block must name a section.
func renderReference(page []byte) ([]byte, error) {
	docs, err := readFieldDocs(".")
	if err != nil {
		return nil, err
	}
	bySection := map[string]section{}
	for _, sec := range serverSections() {
		bySection[sec.key] = sec
	}

	seen := map[string]int{}
	var problems []string
	out := generatedBlock.ReplaceAllFunc(page, func(block []byte) []byte {
		m := generatedBlock.FindSubmatch(block)
		begin, end := string(m[1]), string(m[2])
		seen[begin]++
		if begin != end {
			problems = append(problems, fmt.Sprintf("block %s ends as %s", begin, end))
			return block
		}
		if begin == presetsBlock {
			return []byte("<!-- BEGIN GENERATED: " + begin + " -->\n" + renderPresets() + "<!-- END GENERATED: " + begin + " -->")
		}
		sec, ok := bySection[begin]
		if !ok {
			problems = append(problems, fmt.Sprintf("block %s names no section of Config", begin))
			return block
		}
		for _, r := range sectionRows(sec, docs) {
			if r.problem != "" {
				problems = append(problems, fmt.Sprintf("%s %s (%s): %s", begin, r.key, r.goField, r.problem))
			}
		}
		return []byte("<!-- BEGIN GENERATED: " + begin + " -->\n" + renderSection(sec, docs) + "<!-- END GENERATED: " + begin + " -->")
	})
	for key := range bySection {
		if seen[key] != 1 {
			problems = append(problems, fmt.Sprintf("section %s has %d generated blocks in the page, want 1", key, seen[key]))
		}
	}
	if seen[presetsBlock] != 1 {
		problems = append(problems, fmt.Sprintf("the presets have %d generated blocks in the page, want 1", seen[presetsBlock]))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("docs/configuration.md: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

func renderSection(sec section, docs map[string]string) string {
	var b strings.Builder
	b.WriteString("<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->\n\n")
	for _, f := range shortForms[sec.key] {
		fmt.Fprintf(&b, "Short form: `%s: %s` is the same as `%s: %s`.\n\n", sec.key, f.short, sec.key, f.long)
	}
	b.WriteString("| Key | Type | Meaning | Default |\n|---|---|---|---|\n")
	for _, r := range sectionRows(sec, docs) {
		key := sec.key
		if r.key != "" {
			key = r.key
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", key, r.typ, r.meaning, r.def)
	}
	return b.String()
}

// presetsBlock is the one generated block that is not a server section: the
// upstream presets, rendered from the presets map so that a changed URL or TTL
// changes the page.
const presetsBlock = "presets"

func renderPresets() string {
	var b strings.Builder
	b.WriteString("<!-- Generated from internal/config/presets.go by `make reference`. Edit the presets, not this table. -->\n\n")
	b.WriteString("| Preset | Protocol | URL | `ttl` | `negativeTtl` |\n|---|---|---|---|---|\n")
	for _, name := range PresetNames() {
		p := presets[name]
		protocol := "`" + string(p.Protocol) + "`"
		if !servedProtocol(p.Protocol) {
			protocol += ", reserved: this build does not serve it"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %s |\n", name, protocol, p.URL,
			formatDefault(durationType, reflect.ValueOf(Duration(p.TTL))),
			formatDefault(durationType, reflect.ValueOf(Duration(p.NegativeTTL))))
	}
	return b.String()
}

func firstDifference(want, got []byte) string {
	wl, gl := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := range max(len(wl), len(gl)) {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("first difference at line %d:\n page:      %s\n generated: %s", i+1, w, g)
		}
	}
	return ""
}

// The tables for repositories, lifecycle, defaults, and manifests are written
// by hand, because those sections are read node by node rather than decoded
// into tagged structs. Each table follows a marker
//
//	<!-- keys checked against the loader: <table> -->
//
// and TestReference_HandWrittenTablesListWhatTheLoaderAccepts asks the loader
// which keys it accepts at every mapping, by writing an unknown key there and
// reading the "expected one of" its error lists.

// keyProbe writes the unknown key zzz into one mapping. prefix is where that
// mapping's keys appear in the table.
type keyProbe struct {
	prefix    string
	doc       string
	manifests bool
}

var handTables = map[string][]keyProbe{
	"repos": {
		{prefix: "", doc: "repos:\n  r:\n    zzz: 1\n"},
		{prefix: "lifecycle.", doc: "repos:\n  r:\n    protocol: oci\n    lifecycle: {zzz: 1}\n"},
	},
	"repos.upstream": {
		{prefix: "", doc: "repos:\n  r:\n    upstream: {zzz: 1}\n"},
	},
	"lifecycle": {
		{prefix: "", doc: "lifecycle: {zzz: 1}\n"},
		{prefix: "stages[].", doc: "lifecycle: {stages: [{zzz: 1}]}\n"},
	},
	"defaults": {
		{prefix: "", doc: "defaults: {zzz: 1}\n"},
		{prefix: "hosted.", doc: "defaults: {hosted: {zzz: 1}}\n"},
		{prefix: "proxy.", doc: "defaults: {proxy: {zzz: 1}}\n"},
		{prefix: "virtual.", doc: "defaults: {virtual: {zzz: 1}}\n"},
	},
	"Repository": {
		{prefix: "", doc: "kind: Repository\nzzz: 1\n", manifests: true},
		{prefix: "metadata.", doc: "kind: Repository\nmetadata: {zzz: 1}\n", manifests: true},
		{prefix: "spec.", doc: "kind: Repository\nmetadata: {name: r}\nspec: {zzz: 1}\n", manifests: true},
		{prefix: "spec.upstream.", doc: "kind: Repository\nmetadata: {name: r}\nspec: {type: proxy, protocol: oci, upstream: {zzz: 1}}\n", manifests: true},
		{prefix: "spec.members[].", doc: "kind: Repository\nmetadata: {name: r}\nspec: {type: virtual, protocol: oci, members: [{zzz: 1}]}\n", manifests: true},
		{prefix: "spec.access.", doc: "kind: Repository\nmetadata: {name: r}\nspec: {type: hosted, protocol: oci, access: {zzz: 1}}\n", manifests: true},
		{prefix: "spec.lifecycle.", doc: "kind: Repository\nmetadata: {name: r}\nspec: {type: hosted, protocol: oci, lifecycle: {zzz: 1}}\n", manifests: true},
	},
	"Lifecycle": {
		{prefix: "", doc: "kind: Lifecycle\nzzz: 1\n", manifests: true},
		{prefix: "metadata.", doc: "kind: Lifecycle\nmetadata: {zzz: 1}\n", manifests: true},
		{prefix: "spec.", doc: "kind: Lifecycle\nspec: {zzz: 1}\n", manifests: true},
		{prefix: "spec.stages[].", doc: "kind: Lifecycle\nspec: {stages: [{zzz: 1}]}\n", manifests: true},
	},
	"OrgPolicy": {
		{prefix: "", doc: "kind: OrgPolicy\nzzz: 1\n", manifests: true},
		{prefix: "metadata.", doc: "kind: OrgPolicy\nmetadata: {zzz: 1}\n", manifests: true},
		{prefix: "spec.", doc: "kind: OrgPolicy\nspec: {zzz: 1}\n", manifests: true},
		{prefix: "spec.mandatoryRepos[].", doc: "kind: OrgPolicy\nspec: {mandatoryRepos: [{zzz: 1}]}\n", manifests: true},
		{prefix: "spec.namespaces[].", doc: "kind: OrgPolicy\nspec: {namespaces: [{zzz: 1}]}\n", manifests: true},
	},
}

var expectedOneOf = regexp.MustCompile(`unknown field "zzz", expected one of: ([^\n]+)`)

func TestReference_HandWrittenTablesListWhatTheLoaderAccepts(t *testing.T) {
	page, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatal(err)
	}
	documented, err := handTableKeys(page)
	if err != nil {
		t.Fatal(err)
	}
	for table := range documented {
		if _, ok := handTables[table]; !ok {
			t.Errorf("the page checks table %s, which handTables has no probes for", table)
		}
	}

	for table, probes := range handTables {
		t.Run(table, func(t *testing.T) {
			doc, ok := documented[table]
			if !ok {
				t.Fatalf("no table follows <!-- keys checked against the loader: %s --> in the page", table)
			}

			// A key whose value is a mapping with probes of its own may be
			// documented itself, as `lifecycle` is beside `lifecycle.stages`,
			// but its keys must be.
			accepted, mappings := map[string]bool{}, map[string]bool{}
			for _, p := range probes {
				if p.prefix != "" {
					mappings[strings.TrimSuffix(strings.TrimSuffix(p.prefix, "."), "[]")] = true
				}
				for _, k := range loaderKeys(t, p) {
					accepted[p.prefix+k] = true
				}
			}
			for k := range accepted {
				if !doc[k] && !mappings[k] {
					t.Errorf("the loader accepts %s, but the %s table does not list it", k, table)
				}
			}
			for k := range doc {
				if !accepted[k] {
					t.Errorf("the %s table lists %s, which the loader does not accept", table, k)
				}
			}
		})
	}
}

// loaderKeys returns the keys the loader accepts at the probed mapping.
func loaderKeys(t *testing.T, p keyProbe) []string {
	t.Helper()
	var err error
	if p.manifests {
		_, err = ParseManifests([]byte(p.doc), "probe.yaml")
	} else {
		_, err = Parse([]byte(p.doc), "probe.yaml")
	}
	if err == nil {
		t.Fatalf("probe %q loaded; the loader no longer rejects an unknown key there", p.doc)
	}
	m := expectedOneOf.FindAllStringSubmatch(err.Error(), -1)
	if len(m) != 1 {
		t.Fatalf("probe %q: want one unknown-key error, got: %v", p.doc, err)
	}
	return strings.Split(m[0][1], ", ")
}

var (
	tableMarker = regexp.MustCompile(`^<!-- keys checked against the loader: (\S+) -->$`)
	firstCell   = regexp.MustCompile("^\\| `([^`]+)` \\|")
	codeSpan    = regexp.MustCompile("`([^`]+)`")
)

// handTableKeys reads the first column of the table after each marker. A table
// whose header is "Fields it may set" lists a mapping's keys in its second
// column instead, as the defaults table does.
func handTableKeys(page []byte) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	lines := strings.Split(string(page), "\n")
	for i := 0; i < len(lines); i++ {
		m := tableMarker.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		table := m[1]
		if _, dup := out[table]; dup {
			return nil, fmt.Errorf("table %s is marked twice", table)
		}
		keys := map[string]bool{}
		j := i + 1
		for j < len(lines) && !strings.HasPrefix(lines[j], "|") {
			j++
		}
		if j >= len(lines) {
			return nil, fmt.Errorf("no table follows the marker for %s", table)
		}
		fieldsInSecond := strings.Contains(lines[j], "Fields it may set")
		for j += 2; j < len(lines) && strings.HasPrefix(lines[j], "|"); j++ {
			cell := firstCell.FindStringSubmatch(lines[j])
			if cell == nil {
				return nil, fmt.Errorf("table %s: row %q does not start with a key in backticks", table, lines[j])
			}
			keys[cell[1]] = true
			if fieldsInSecond {
				second := strings.SplitN(lines[j], "|", 4)[2]
				for _, f := range codeSpan.FindAllStringSubmatch(second, -1) {
					keys[cell[1]+"."+f[1]] = true
				}
			}
		}
		out[table] = keys
		i = j
	}
	return out, nil
}
