package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spj/sindri/internal/model"
)

// Manifests is the desired state the reconciler converges the database onto:
// the long form of ARCHITECTURE.md section 8.
//
// The short form in sindri.yaml and a directory of enveloped documents both
// load into this one type. Desugaring is one-way and total, so nothing that
// receives a Manifests can tell, or may ask, which spelling produced it. The
// golden test TestManifests_ShortAndLongFormsAreIdentical keeps that true.
type Manifests struct {
	OrgPolicy    *OrgPolicy   `json:"orgPolicy,omitempty"`
	Lifecycle    *Lifecycle   `json:"lifecycle,omitempty"`
	Repositories []Repository `json:"repositories"`
}

// Repository is one `kind: Repository` document.
type Repository struct {
	// Namespace is the owning team, or empty for an organisation-level
	// repository. A namespaced repository's full name is "namespace/name", so
	// the namespace is visible in every URL that reaches it.
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Spec      RepositorySpec `json:"spec"`

	// Source locates the document for error messages. It is not part of the
	// desired state, and the golden comparisons ignore it.
	Source Source `json:"source"`
}

// FullName is the repository's name in the database and on the wire.
func (r Repository) FullName() string {
	return QualifiedName(r.Namespace, r.Name)
}

// QualifiedName joins a namespace and a local name.
func QualifiedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// RepositorySpec is the explicit form of a repository. Type and protocol are
// always present after loading, whatever was inferred.
type RepositorySpec struct {
	Type      model.RepoType `json:"type"`
	Protocol  model.Protocol `json:"protocol"`
	Upstream  *Upstream      `json:"upstream,omitempty"`
	Members   []MemberRef    `json:"members,omitempty"`
	Access    Access         `json:"access"`
	Lifecycle *RepoLifecycle `json:"lifecycle,omitempty"`
}

// Upstream is a proxy repository's source, with any preset already expanded.
type Upstream struct {
	URL         string   `json:"url"`
	TTL         Duration `json:"ttl"`
	NegativeTTL Duration `json:"negativeTtl"`
	// Credentials names an entry of the server's credentials section. The
	// secret itself never appears in a manifest (ADR 15).
	Credentials string `json:"credentials,omitempty"`
}

// MemberRef names a member of a virtual repository as "<type>/<ref>", for
// example "hosted/internal-py". The type prefix keeps the three repository
// types visible in the explicit form, which is the point of invariant I5. The
// ref after it resolves inside the document's namespace, or reaches across a
// boundary with a prefix: "org/<name>" for an organisation repository,
// "<team>/<name>" for another team's.
type MemberRef struct {
	Ref string `json:"ref" yaml:"ref"`
}

// Split returns the member's declared type and its reference.
func (m MemberRef) Split() (model.RepoType, string) {
	typ, rest, _ := strings.Cut(m.Ref, "/")
	return model.RepoType(typ), rest
}

// Access lists who may read and write a repository. Entries are group names as
// the identity provider reports them, such as "team:payments", or the keyword
// "authenticated". Membership is resolved at request time and never stored
// (ADR 5). Write implies read.
type Access struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
}

// AccessAuthenticated grants a role to any caller with a valid identity.
// There is no anonymous grant in v1 (ADR 18).
const AccessAuthenticated = "authenticated"

// RepoLifecycle restricts which stages a hosted repository may promote into.
// Empty means every stage. Lifecycle on a proxy or virtual repository is an
// error (invariant I4).
type RepoLifecycle struct {
	Stages []string `json:"stages"`
}

// PolicyMode is how much autonomy teams have.
type PolicyMode string

const (
	// PolicyCentralized means only organisation-level documents may declare
	// repositories.
	PolicyCentralized PolicyMode = "centralized"
	// PolicyDelegated means teams declare repositories in their namespaces
	// freely. It is the mode an absent OrgPolicy implies.
	PolicyDelegated PolicyMode = "delegated"
	// PolicyHybrid means teams declare their own repositories, and the
	// organisation's mandatory repositories must exist.
	PolicyHybrid PolicyMode = "hybrid"
)

// OrgPolicy is the `kind: OrgPolicy` document. Absent means delegated with no
// mandatory repositories, so a single team never writes one.
type OrgPolicy struct {
	Mode           PolicyMode        `json:"mode"`
	MandatoryRepos []MandatoryRepo   `json:"mandatoryRepos,omitempty"`
	Namespaces     []NamespacePolicy `json:"namespaces,omitempty"`
	Source         Source            `json:"source"`
}

// MandatoryRepo is an organisation repository that must exist. A locked one can
// be referenced by teams and never overridden by them.
type MandatoryRepo struct {
	Name   string `json:"name"`
	Locked bool   `json:"locked,omitempty"`
}

// NamespacePolicy names who may apply a team's manifests.
type NamespacePolicy struct {
	Name   string   `json:"name"`
	Admins []string `json:"admins,omitempty"`
}

// EffectiveOrgPolicy returns the policy, or the delegated default when absent.
func (m Manifests) EffectiveOrgPolicy() OrgPolicy {
	if m.OrgPolicy == nil {
		return OrgPolicy{Mode: PolicyDelegated}
	}
	return *m.OrgPolicy
}

// Lifecycle is the `kind: Lifecycle` document: the ordered stages and the
// evidence each requires.
type Lifecycle struct {
	Stages []Stage `json:"stages"`
	Source Source  `json:"source"`
}

// Stage is one lifecycle stage and its gates.
type Stage struct {
	Name  string               `json:"name"`
	Gates []model.EvidenceType `json:"gates,omitempty"`
}

// Source records where a document came from, for error messages only.
type Source struct {
	File   string         `json:"file,omitempty"`
	Line   int            `json:"line,omitempty"`
	Path   string         `json:"path,omitempty"`
	Fields map[string]Loc `json:"fields,omitempty"`
}

// Loc is the position and displayed path of one field.
type Loc struct {
	Line int    `json:"line"`
	Path string `json:"path"`
}

// At locates a field of the document, falling back to the document itself.
func (s Source) At(field string) (int, string) {
	if loc, ok := s.Fields[field]; ok {
		return loc.Line, loc.Path
	}
	if field == "" {
		return s.Line, s.Path
	}
	return s.Line, join(s.Path, field)
}

// Errorf builds a located error for a field of this document.
func (s Source) Errorf(field, format string, args ...any) *FieldError {
	line, path := s.At(field)
	return &FieldError{File: s.File, Line: line, Field: path, Msg: fmt.Sprintf(format, args...)}
}

func (s *Source) set(field string, line int, path string) {
	if s.Fields == nil {
		s.Fields = map[string]Loc{}
	}
	s.Fields[field] = Loc{Line: line, Path: path}
}

// The document kinds of the long form.
const (
	KindRepository = "Repository"
	KindOrgPolicy  = "OrgPolicy"
	KindLifecycle  = "Lifecycle"
)

// LoadManifests reads the long form from a file or a directory of *.yaml and
// *.yml files. Each file may hold several documents separated by ---.
func LoadManifests(path string) (*Manifests, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifests %s: %w", path, err)
	}
	files := []string{path}
	if info.IsDir() {
		files, err = manifestFiles(path)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("reading manifests %s: no .yaml or .yml files", path)
		}
	}

	var (
		out  Manifests
		list Errors
	)
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the operator names their own manifest directory
		if err != nil {
			return nil, fmt.Errorf("reading manifest %s: %w", f, err)
		}
		if err := parseManifestsInto(&out, raw, f); err != nil {
			list = append(list, AsErrors(err)...)
		}
	}
	if err := list.Err(); err != nil {
		return nil, err
	}
	out.normalize()
	return &out, nil
}

// ParseManifests decodes long-form documents from raw.
func ParseManifests(raw []byte, name string) (*Manifests, error) {
	var out Manifests
	if err := parseManifestsInto(&out, raw, name); err != nil {
		return nil, err
	}
	out.normalize()
	return &out, nil
}

func manifestFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml":
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing manifests in %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}

// LooksLikeManifests reports whether raw holds enveloped documents rather than
// a sindri.yaml, so one -f flag can take either.
func LooksLikeManifests(raw []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			return false
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		return root.Kind == yaml.MappingNode && hasKey(root, "kind")
	}
}

func parseManifestsInto(out *Manifests, raw []byte, file string) error {
	c := &errs{file: file}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.add(0, "", "parsing: %s", cleanYAMLError(err))
			break
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if err := expandNode(root, os.LookupEnv); err != nil {
			c.add(0, "", "%s", err)
			continue
		}
		parseDocument(c, out, root)
	}
	return c.list.Err()
}

func parseDocument(c *errs, out *Manifests, root *yaml.Node) {
	env, ok := readMapping(c, root, "", "kind", "metadata", "spec")
	if !ok {
		return
	}
	if !env.has("kind") {
		c.add(root.Line, "kind", "every manifest document needs a kind: %s, %s, or %s",
			KindRepository, KindOrgPolicy, KindLifecycle)
		return
	}
	kind, _ := readString(c, env.value["kind"], "kind")
	switch kind {
	case KindRepository:
		parseRepositoryDoc(c, out, env)
	case KindOrgPolicy:
		parseOrgPolicyDoc(c, out, env)
	case KindLifecycle:
		parseLifecycleDoc(c, out, env)
	default:
		c.add(env.lineOf("kind"), "kind", "%q is not a kind, want %s, %s, or %s",
			kind, KindRepository, KindOrgPolicy, KindLifecycle)
	}
}

func parseRepositoryDoc(c *errs, out *Manifests, env mapping) {
	if !env.has("metadata") {
		c.add(env.node.Line, "metadata", "a Repository needs metadata.name")
		return
	}
	meta, ok := readMapping(c, env.value["metadata"], "metadata", "name", "namespace")
	if !ok {
		return
	}
	var repo Repository
	if meta.has("name") {
		repo.Name, _ = readString(c, meta.value["name"], "metadata.name")
	}
	if meta.has("namespace") {
		repo.Namespace, _ = readString(c, meta.value["namespace"], "metadata.namespace")
	}
	if repo.Name == "" {
		c.add(meta.lineOf("name"), "metadata.name", "is required")
		return
	}
	prefix := fmt.Sprintf("Repository[%s]", QualifiedName(repo.Namespace, repo.Name))
	repo.Source = Source{File: c.file, Line: meta.lineOf("name"), Path: prefix}

	if !env.has("spec") {
		c.add(env.node.Line, prefix+".spec", "is required")
		return
	}
	spec, ok := readMapping(c, env.value["spec"], prefix+".spec",
		"type", "protocol", "upstream", "members", "access", "lifecycle")
	if !ok {
		return
	}
	path := func(f string) string { return prefix + ".spec." + f }
	for _, k := range spec.keys {
		repo.Source.set(k, spec.lineOf(k), path(k))
	}

	if spec.has("type") {
		s, _ := readString(c, spec.value["type"], path("type"))
		repo.Spec.Type = model.RepoType(s)
	} else {
		c.add(spec.node.Line, path("type"), "is required in the long form: hosted, proxy, or virtual")
	}
	if spec.has("protocol") {
		s, _ := readString(c, spec.value["protocol"], path("protocol"))
		repo.Spec.Protocol = model.Protocol(s)
	} else {
		c.add(spec.node.Line, path("protocol"), "is required in the long form: oci or pypi")
	}
	if spec.has("upstream") {
		repo.Spec.Upstream = parseLongUpstream(c, spec.value["upstream"], path("upstream"), &repo.Source)
	}
	if spec.has("members") {
		repo.Spec.Members = parseLongMembers(c, spec.value["members"], path("members"), &repo.Source)
	}
	if spec.has("access") {
		repo.Spec.Access = parseAccess(c, spec.value["access"], path("access"), &repo.Source, "read", "write")
	}
	if spec.has("lifecycle") {
		repo.Spec.Lifecycle = parseRepoLifecycle(c, spec.value["lifecycle"], path("lifecycle"))
	}
	out.Repositories = append(out.Repositories, repo)
}

func parseLongUpstream(c *errs, n *yaml.Node, field string, src *Source) *Upstream {
	m, ok := readMapping(c, n, field, "url", "ttl", "negativeTtl", "credentials")
	if !ok {
		return nil
	}
	up := &Upstream{}
	for _, k := range m.keys {
		src.set("upstream."+k, m.lineOf(k), field+"."+k)
	}
	if m.has("url") {
		up.URL, _ = readString(c, m.value["url"], field+".url")
	} else {
		c.add(m.node.Line, field+".url", "is required")
	}
	if m.has("ttl") {
		up.TTL, _ = readDuration(c, m.value["ttl"], field+".ttl")
	}
	if m.has("negativeTtl") {
		up.NegativeTTL, _ = readDuration(c, m.value["negativeTtl"], field+".negativeTtl")
	}
	if m.has("credentials") {
		up.Credentials, _ = readString(c, m.value["credentials"], field+".credentials")
	}
	return up
}

func parseLongMembers(c *errs, n *yaml.Node, field string, src *Source) []MemberRef {
	n = resolveAlias(n)
	if n.Kind != yaml.SequenceNode {
		c.add(n.Line, field, "expected a list, got %s", kindName(n.Kind))
		return nil
	}
	out := make([]MemberRef, 0, len(n.Content))
	for i, item := range n.Content {
		f := fmt.Sprintf("%s[%d]", field, i)
		m, ok := readMapping(c, item, f, "ref")
		if !ok {
			continue
		}
		if !m.has("ref") {
			c.add(item.Line, f+".ref", "is required, for example hosted/internal-py")
			continue
		}
		ref, _ := readString(c, m.value["ref"], f+".ref")
		src.set(fmt.Sprintf("members[%d]", i), item.Line, f)
		out = append(out, MemberRef{Ref: ref})
	}
	return out
}

func parseAccess(c *errs, n *yaml.Node, field string, src *Source, readKey, writeKey string) Access {
	m, ok := readMapping(c, n, field, readKey, writeKey)
	if !ok {
		return Access{}
	}
	var a Access
	if m.has(readKey) {
		src.set("access.read", m.lineOf(readKey), field+"."+readKey)
		a.Read, _ = readStrings(c, m.value[readKey], field+"."+readKey)
	}
	if m.has(writeKey) {
		src.set("access.write", m.lineOf(writeKey), field+"."+writeKey)
		a.Write, _ = readStrings(c, m.value[writeKey], field+"."+writeKey)
	}
	return a
}

// parseRepoLifecycle accepts `lifecycle: {stages: [...]}` or, as sugar,
// `lifecycle: [...]`.
func parseRepoLifecycle(c *errs, n *yaml.Node, field string) *RepoLifecycle {
	n = resolveAlias(n)
	if n.Kind == yaml.SequenceNode {
		stages, _ := readStrings(c, n, field)
		return &RepoLifecycle{Stages: stages}
	}
	m, ok := readMapping(c, n, field, "stages")
	if !ok {
		return nil
	}
	lc := &RepoLifecycle{Stages: []string{}}
	if m.has("stages") {
		lc.Stages, _ = readStrings(c, m.value["stages"], field+".stages")
	}
	return lc
}

func parseOrgPolicyDoc(c *errs, out *Manifests, env mapping) {
	if env.has("metadata") {
		// Accepted for symmetry with the other kinds; an organisation has one
		// policy, so the name carries no meaning.
		readMapping(c, env.value["metadata"], "OrgPolicy.metadata", "name")
	}
	if out.OrgPolicy != nil {
		c.add(env.node.Line, "OrgPolicy", "declared twice; an organisation has one policy (first in %s:%d)",
			out.OrgPolicy.Source.File, out.OrgPolicy.Source.Line)
		return
	}
	p := &OrgPolicy{Mode: PolicyDelegated, Source: Source{File: c.file, Line: env.node.Line, Path: "OrgPolicy.spec"}}
	if !env.has("spec") {
		out.OrgPolicy = p
		return
	}
	spec, ok := readMapping(c, env.value["spec"], "OrgPolicy.spec", "mode", "mandatoryRepos", "namespaces")
	if !ok {
		return
	}
	for _, k := range spec.keys {
		p.Source.set(k, spec.lineOf(k), "OrgPolicy.spec."+k)
	}
	if spec.has("mode") {
		mode, _ := readString(c, spec.value["mode"], "OrgPolicy.spec.mode")
		switch PolicyMode(mode) {
		case PolicyCentralized, PolicyDelegated, PolicyHybrid:
			p.Mode = PolicyMode(mode)
		default:
			c.add(spec.lineOf("mode"), "OrgPolicy.spec.mode", "%q is not a mode, want centralized, delegated, or hybrid", mode)
		}
	}
	if spec.has("mandatoryRepos") {
		n := resolveAlias(spec.value["mandatoryRepos"])
		if n.Kind != yaml.SequenceNode {
			c.add(n.Line, "OrgPolicy.spec.mandatoryRepos", "expected a list, got %s", kindName(n.Kind))
		} else {
			for i, item := range n.Content {
				f := fmt.Sprintf("OrgPolicy.spec.mandatoryRepos[%d]", i)
				m, ok := readMapping(c, item, f, "name", "locked")
				if !ok {
					continue
				}
				var mr MandatoryRepo
				if m.has("name") {
					mr.Name, _ = readString(c, m.value["name"], f+".name")
				}
				if mr.Name == "" {
					c.add(item.Line, f+".name", "is required")
					continue
				}
				if m.has("locked") {
					mr.Locked, _ = readBool(c, m.value["locked"], f+".locked")
				}
				p.Source.set(fmt.Sprintf("mandatoryRepos[%d]", i), item.Line, f)
				p.MandatoryRepos = append(p.MandatoryRepos, mr)
			}
		}
	}
	if spec.has("namespaces") {
		n := resolveAlias(spec.value["namespaces"])
		if n.Kind != yaml.SequenceNode {
			c.add(n.Line, "OrgPolicy.spec.namespaces", "expected a list, got %s", kindName(n.Kind))
		} else {
			for i, item := range n.Content {
				f := fmt.Sprintf("OrgPolicy.spec.namespaces[%d]", i)
				m, ok := readMapping(c, item, f, "name", "admins")
				if !ok {
					continue
				}
				var np NamespacePolicy
				if m.has("name") {
					np.Name, _ = readString(c, m.value["name"], f+".name")
				}
				if np.Name == "" {
					c.add(item.Line, f+".name", "is required")
					continue
				}
				if m.has("admins") {
					np.Admins, _ = readStrings(c, m.value["admins"], f+".admins")
				}
				p.Source.set(fmt.Sprintf("namespaces[%d]", i), item.Line, f)
				p.Namespaces = append(p.Namespaces, np)
			}
		}
	}
	out.OrgPolicy = p
}

func parseLifecycleDoc(c *errs, out *Manifests, env mapping) {
	if env.has("metadata") {
		readMapping(c, env.value["metadata"], "Lifecycle.metadata", "name")
	}
	if out.Lifecycle != nil {
		c.add(env.node.Line, "Lifecycle", "declared twice; stages are organisation-wide (first in %s:%d)",
			out.Lifecycle.Source.File, out.Lifecycle.Source.Line)
		return
	}
	lc := &Lifecycle{Stages: []Stage{}, Source: Source{File: c.file, Line: env.node.Line, Path: "Lifecycle.spec"}}
	if !env.has("spec") {
		out.Lifecycle = lc
		return
	}
	spec, ok := readMapping(c, env.value["spec"], "Lifecycle.spec", "stages")
	if !ok {
		return
	}
	if spec.has("stages") {
		n := resolveAlias(spec.value["stages"])
		if n.Kind != yaml.SequenceNode {
			c.add(n.Line, "Lifecycle.spec.stages", "expected a list, got %s", kindName(n.Kind))
		} else {
			for i, item := range n.Content {
				f := fmt.Sprintf("Lifecycle.spec.stages[%d]", i)
				m, ok := readMapping(c, item, f, "name", "gates")
				if !ok {
					continue
				}
				var st Stage
				if m.has("name") {
					st.Name, _ = readString(c, m.value["name"], f+".name")
				}
				if m.has("gates") {
					gates, _ := readStrings(c, m.value["gates"], f+".gates")
					for _, g := range gates {
						st.Gates = append(st.Gates, model.EvidenceType(g))
					}
				}
				lc.Source.set(fmt.Sprintf("stages[%d]", i), item.Line, f)
				lc.Stages = append(lc.Stages, st)
			}
		}
	}
	out.Lifecycle = lc
}

// normalize puts the tree in its canonical shape: repositories sorted by full
// name, and empty lists represented one way. Two spellings of one setup then
// compare equal field by field.
func (m *Manifests) normalize() {
	sort.SliceStable(m.Repositories, func(i, j int) bool {
		return m.Repositories[i].FullName() < m.Repositories[j].FullName()
	})
	if m.Repositories == nil {
		m.Repositories = []Repository{}
	}
	for i := range m.Repositories {
		spec := &m.Repositories[i].Spec
		if len(spec.Members) == 0 {
			spec.Members = nil
		}
		if len(spec.Access.Read) == 0 {
			spec.Access.Read = nil
		}
		if len(spec.Access.Write) == 0 {
			spec.Access.Write = nil
		}
	}
	if m.Lifecycle != nil {
		for i := range m.Lifecycle.Stages {
			if len(m.Lifecycle.Stages[i].Gates) == 0 {
				m.Lifecycle.Stages[i].Gates = nil
			}
		}
	}
}
