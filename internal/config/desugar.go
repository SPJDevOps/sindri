package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spj/sindri/internal/model"
)

// The sections of sindri.yaml that hold desired state rather than server
// settings. Parse lifts them out of the tree before the server sections are
// decoded and desugars them into Manifests.
const (
	sectionRepos     = "repos"
	sectionLifecycle = "lifecycle"
	sectionDefaults  = "defaults"
)

var shortSections = []string{sectionRepos, sectionLifecycle, sectionDefaults}

// shortNodes are the desired-state sections of one sindri.yaml, as written.
type shortNodes struct {
	repos, lifecycle, defaults *yaml.Node
	lines                      map[string]int
}

func (s shortNodes) present() bool {
	return s.repos != nil || s.lifecycle != nil || s.defaults != nil
}

// liftShortSections removes the desired-state keys from the root mapping and
// returns them, so the server sections decode exactly as they did before M7.
func liftShortSections(root *yaml.Node) shortNodes {
	out := shortNodes{lines: map[string]int{}}
	if root.Kind != yaml.MappingNode {
		return out
	}
	kept := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		switch key.Value {
		case sectionRepos:
			out.repos = val
		case sectionLifecycle:
			out.lifecycle = val
		case sectionDefaults:
			out.defaults = val
		default:
			kept = append(kept, key, val)
			continue
		}
		out.lines[key.Value] = key.Line
	}
	root.Content = kept
	return out
}

// typeDefaults is one entry of the defaults section. Each field applies only
// where the repository itself does not set it. There is deliberately no
// `extends:` (ADR 7); YAML anchors cover the rest.
type typeDefaults struct {
	access      Access
	readSet     bool
	writeSet    bool
	ttl         *Duration
	negativeTTL *Duration
	credentials *string
	lifecycle   *RepoLifecycle
}

// desugarShort turns the short form into the long form. It is the only place
// that knows the short spelling exists.
func desugarShort(file string, s shortNodes) (*Manifests, error) {
	c := &errs{file: file}
	out := &Manifests{Repositories: []Repository{}}

	defaults := map[model.RepoType]typeDefaults{}
	if s.defaults != nil {
		defaults = desugarDefaults(c, s.defaults)
	}
	if s.lifecycle != nil {
		out.Lifecycle = desugarLifecycle(c, s.lifecycle, s.lines[sectionLifecycle])
	}
	if s.repos != nil {
		out.Repositories = desugarRepos(c, s.repos, defaults)
	}
	if err := c.list.Err(); err != nil {
		return nil, err
	}
	out.normalize()
	return out, nil
}

func desugarDefaults(c *errs, n *yaml.Node) map[model.RepoType]typeDefaults {
	out := map[model.RepoType]typeDefaults{}
	m, ok := readMapping(c, n, sectionDefaults, string(model.RepoTypeHosted), string(model.RepoTypeProxy), string(model.RepoTypeVirtual))
	if !ok {
		return out
	}
	for _, typ := range m.keys {
		field := sectionDefaults + "." + typ
		allowed := []string{"read", "write"}
		switch model.RepoType(typ) {
		case model.RepoTypeProxy:
			allowed = append(allowed, "ttl", "negativeTtl", "credentials")
		case model.RepoTypeHosted:
			allowed = append(allowed, "lifecycle")
		}
		dm, ok := readMapping(c, m.value[typ], field, allowed...)
		if !ok {
			continue
		}
		var d typeDefaults
		if dm.has("read") {
			d.access.Read, _ = readStrings(c, dm.value["read"], field+".read")
			d.readSet = true
		}
		if dm.has("write") {
			d.access.Write, _ = readStrings(c, dm.value["write"], field+".write")
			d.writeSet = true
		}
		if dm.has("ttl") {
			v, _ := readDuration(c, dm.value["ttl"], field+".ttl")
			d.ttl = &v
		}
		if dm.has("negativeTtl") {
			v, _ := readDuration(c, dm.value["negativeTtl"], field+".negativeTtl")
			d.negativeTTL = &v
		}
		if dm.has("credentials") {
			v, _ := readString(c, dm.value["credentials"], field+".credentials")
			d.credentials = &v
		}
		if dm.has("lifecycle") {
			d.lifecycle = parseRepoLifecycle(c, dm.value["lifecycle"], field+".lifecycle")
		}
		out[model.RepoType(typ)] = d
	}
	return out
}

// desugarLifecycle accepts
//
//	lifecycle:
//	  stages: [dev, integration, prod]
//	  gates:
//	    prod: [sbom, signature]
//
// and also stage entries written as {name, gates}, which is what the long form
// uses.
func desugarLifecycle(c *errs, n *yaml.Node, line int) *Lifecycle {
	lc := &Lifecycle{Stages: []Stage{}, Source: Source{File: c.file, Line: line, Path: sectionLifecycle}}
	m, ok := readMapping(c, n, sectionLifecycle, "stages", "gates")
	if !ok {
		return lc
	}
	index := map[string]int{}
	explicitGates := map[string]bool{}
	if m.has("stages") {
		sn := resolveAlias(m.value["stages"])
		if sn.Kind != yaml.SequenceNode {
			c.add(sn.Line, "lifecycle.stages", "expected a list, got %s", kindName(sn.Kind))
		} else {
			for i, item := range sn.Content {
				field := fmt.Sprintf("lifecycle.stages[%d]", i)
				var st Stage
				if r := resolveAlias(item); r.Kind == yaml.MappingNode {
					sm, ok := readMapping(c, r, field, "name", "gates")
					if !ok {
						continue
					}
					if sm.has("name") {
						st.Name, _ = readString(c, sm.value["name"], field+".name")
					}
					if sm.has("gates") {
						gates, _ := readStrings(c, sm.value["gates"], field+".gates")
						for _, g := range gates {
							st.Gates = append(st.Gates, model.EvidenceType(g))
						}
						explicitGates[st.Name] = true
					}
				} else {
					st.Name, _ = readString(c, item, field)
				}
				lc.Source.set(fmt.Sprintf("stages[%d]", i), item.Line, field)
				index[st.Name] = len(lc.Stages)
				lc.Stages = append(lc.Stages, st)
			}
		}
	}
	if m.has("gates") {
		gm, ok := readMapping(c, m.value["gates"], "lifecycle.gates")
		if ok {
			for _, stage := range gm.keys {
				field := "lifecycle.gates." + stage
				i, declared := index[stage]
				if !declared {
					c.add(gm.lineOf(stage), field, "names stage %q, which lifecycle.stages does not declare", stage)
					continue
				}
				if explicitGates[stage] {
					c.add(gm.lineOf(stage), field, "stage %q already lists its gates in lifecycle.stages; write them in one place", stage)
					continue
				}
				gates, _ := readStrings(c, gm.value[stage], field)
				for _, g := range gates {
					lc.Stages[i].Gates = append(lc.Stages[i].Gates, model.EvidenceType(g))
				}
				lc.Source.set(fmt.Sprintf("stages[%d]", i), gm.lineOf(stage), field)
			}
		}
	}
	return lc
}

// shortRepo is one entry of repos: as written, before inference.
type shortRepo struct {
	name   string
	m      mapping
	typ    model.RepoType
	proto  model.Protocol
	src    Source
	failed bool
}

func desugarRepos(c *errs, n *yaml.Node, defaults map[model.RepoType]typeDefaults) []Repository {
	m, ok := readMapping(c, n, sectionRepos)
	if !ok {
		return nil
	}

	// First pass: infer each repository's type, and the protocol of every
	// hosted and proxy repository, so that a virtual repository can infer its
	// protocol from its members in the second pass.
	entries := make([]*shortRepo, 0, len(m.keys))
	byName := map[string]*shortRepo{}
	for _, name := range m.keys {
		field := sectionRepos + "." + name
		e := &shortRepo{name: name, src: Source{File: c.file, Line: m.lineOf(name), Path: field}}
		rm, ok := readMapping(c, m.value[name], field,
			"type", "protocol", "upstream", "members", "read", "write", "lifecycle")
		if !ok {
			e.failed = true
		}
		e.m = rm
		for _, k := range rm.keys {
			long := k
			switch k {
			case "read", "write":
				long = "access." + k
			}
			e.src.set(long, rm.lineOf(k), field+"."+k)
		}
		inferType(c, e)
		entries = append(entries, e)
		byName[name] = e
	}

	out := make([]Repository, 0, len(entries))
	for _, e := range entries {
		if e.failed {
			continue
		}
		repo, ok := desugarRepo(c, e, byName, defaults[e.typ])
		if ok {
			out = append(out, repo)
		}
	}
	return out
}

// inferType applies the inference table of ARCHITECTURE.md section 8.2:
// members means virtual, upstream means proxy, neither means hosted, and both
// is a conflict. An explicit type stays valid but must agree.
func inferType(c *errs, e *shortRepo) {
	hasMembers, hasUpstream := e.m.has("members"), e.m.has("upstream")
	field := e.src.Path
	if hasMembers && hasUpstream {
		c.add(e.m.lineOf("upstream"), field,
			"has both members (line %d) and upstream (line %d): members makes a virtual repository and upstream a proxy; declare them as two repositories",
			e.m.lineOf("members"), e.m.lineOf("upstream"))
		e.failed = true
		return
	}
	inferred := model.RepoTypeHosted
	switch {
	case hasMembers:
		inferred = model.RepoTypeVirtual
	case hasUpstream:
		inferred = model.RepoTypeProxy
	}
	e.typ = inferred
	if e.m.has("type") {
		s, _ := readString(c, e.m.value["type"], field+".type")
		explicit := model.RepoType(s)
		switch explicit {
		case model.RepoTypeHosted, model.RepoTypeProxy, model.RepoTypeVirtual:
		default:
			c.add(e.m.lineOf("type"), field+".type", "%q is not a type, want hosted, proxy, or virtual", s)
			e.failed = true
			return
		}
		if explicit != inferred {
			c.add(e.m.lineOf("type"), field+".type", "says %s, but %s", explicit, inferenceReason(inferred))
			e.failed = true
			return
		}
	}

	if e.m.has("protocol") {
		s, _ := readString(c, e.m.value["protocol"], field+".protocol")
		e.proto = model.Protocol(s)
	}
	if e.typ == model.RepoTypeProxy && e.proto == "" {
		e.proto = upstreamProtocol(e.m.value["upstream"])
	}
}

func inferenceReason(t model.RepoType) string {
	switch t {
	case model.RepoTypeVirtual:
		return "members makes it virtual"
	case model.RepoTypeProxy:
		return "upstream makes it proxy"
	default:
		return "with neither members nor upstream it is hosted"
	}
}

// upstreamProtocol reads the protocol a preset implies, without reporting
// errors; desugarRepo reports them.
func upstreamProtocol(n *yaml.Node) model.Protocol {
	n = resolveAlias(n)
	name := ""
	switch n.Kind {
	case yaml.ScalarNode:
		name = n.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "preset" {
				name = n.Content[i+1].Value
			}
		}
	}
	if p, ok := LookupPreset(strings.TrimSpace(name)); ok {
		return p.Protocol
	}
	return ""
}

func desugarRepo(c *errs, e *shortRepo, byName map[string]*shortRepo, d typeDefaults) (Repository, bool) {
	field := e.src.Path
	repo := Repository{Name: e.name, Source: e.src}
	repo.Spec.Type = e.typ
	before := len(c.list)

	switch e.typ {
	case model.RepoTypeHosted:
		if e.proto == "" {
			c.add(e.src.Line, field+".protocol", "a hosted repository needs a protocol: oci or pypi")
		}
	case model.RepoTypeProxy:
		repo.Spec.Upstream = desugarUpstream(c, e, d)
	case model.RepoTypeVirtual:
		repo.Spec.Members, e.proto = desugarMembers(c, e, byName)
	}
	repo.Spec.Protocol = e.proto

	repo.Spec.Access = d.access
	if !d.readSet {
		repo.Spec.Access.Read = nil
	}
	if !d.writeSet {
		repo.Spec.Access.Write = nil
	}
	if e.m.has("read") {
		repo.Spec.Access.Read, _ = readStrings(c, e.m.value["read"], field+".read")
	}
	if e.m.has("write") {
		repo.Spec.Access.Write, _ = readStrings(c, e.m.value["write"], field+".write")
	}
	if d.lifecycle != nil {
		lc := *d.lifecycle
		repo.Spec.Lifecycle = &lc
	}
	if e.m.has("lifecycle") {
		repo.Spec.Lifecycle = parseRepoLifecycle(c, e.m.value["lifecycle"], field+".lifecycle")
	}
	return repo, len(c.list) == before
}

func desugarUpstream(c *errs, e *shortRepo, d typeDefaults) *Upstream {
	field := e.src.Path + ".upstream"
	n := resolveAlias(e.m.value["upstream"])
	up := &Upstream{}
	var (
		presetName string
		explicit   mapping
		isObject   bool
	)
	switch n.Kind {
	case yaml.ScalarNode:
		presetName = strings.TrimSpace(n.Value)
	case yaml.MappingNode:
		m, ok := readMapping(c, n, field, "preset", "url", "ttl", "negativeTtl", "credentials")
		if !ok {
			return nil
		}
		explicit, isObject = m, true
		if m.has("preset") {
			presetName, _ = readString(c, m.value["preset"], field+".preset")
		}
		for _, k := range m.keys {
			e.src.set("upstream."+k, m.lineOf(k), field+"."+k)
		}
	default:
		c.add(n.Line, field, "expected a preset name or a mapping, got %s", kindName(n.Kind))
		return nil
	}

	// Precedence, lowest first: preset, then defaults.proxy, then what the
	// repository writes. The operator's statement beats the binary's.
	if presetName != "" {
		p, ok := LookupPreset(presetName)
		if !ok {
			c.add(n.Line, field, "%q is not a preset, want one of: %s, or a mapping with url",
				presetName, strings.Join(PresetNames(), ", "))
			return nil
		}
		if !servedProtocol(p.Protocol) {
			c.add(n.Line, field, "preset %q is for %s, which this build does not serve", presetName, p.Protocol)
			return nil
		}
		up.URL = p.URL
		up.TTL = Duration(p.TTL)
		up.NegativeTTL = Duration(p.NegativeTTL)
		if e.proto != "" && e.proto != p.Protocol {
			c.add(e.m.lineOf("protocol"), e.src.Path+".protocol",
				"is %s, but preset %q is %s", e.proto, presetName, p.Protocol)
		}
		e.proto = p.Protocol
	}
	if d.ttl != nil {
		up.TTL = *d.ttl
	}
	if d.negativeTTL != nil {
		up.NegativeTTL = *d.negativeTTL
	}
	if d.credentials != nil {
		up.Credentials = *d.credentials
	}
	if isObject {
		if explicit.has("url") {
			up.URL, _ = readString(c, explicit.value["url"], field+".url")
		}
		if explicit.has("ttl") {
			up.TTL, _ = readDuration(c, explicit.value["ttl"], field+".ttl")
		}
		if explicit.has("negativeTtl") {
			up.NegativeTTL, _ = readDuration(c, explicit.value["negativeTtl"], field+".negativeTtl")
		}
		if explicit.has("credentials") {
			up.Credentials, _ = readString(c, explicit.value["credentials"], field+".credentials")
		}
	}
	if up.URL == "" {
		c.add(n.Line, field, "needs a preset or a url")
		return nil
	}
	if e.proto == "" {
		c.add(e.src.Line, e.src.Path+".protocol", "a proxy with an explicit url needs a protocol: oci or pypi")
	}
	return up
}

var typePrefixes = []model.RepoType{model.RepoTypeHosted, model.RepoTypeProxy, model.RepoTypeVirtual}

// qualifiedMemberType reports the type prefix of a member that is already
// written as "<type>/<ref>".
func qualifiedMemberType(ref string) (model.RepoType, bool) {
	prefix, _, ok := strings.Cut(ref, "/")
	if !ok {
		return "", false
	}
	for _, t := range typePrefixes {
		if model.RepoType(prefix) == t {
			return t, true
		}
	}
	return "", false
}

// desugarMembers qualifies each bare member with its type, and infers the
// virtual repository's protocol from its members. All members must share one
// protocol: one URL serves one wire protocol (ARCHITECTURE.md section 8.2).
func desugarMembers(c *errs, e *shortRepo, byName map[string]*shortRepo) ([]MemberRef, model.Protocol) {
	field := e.src.Path + ".members"
	names, ok := readStrings(c, e.m.value["members"], field)
	if !ok {
		return nil, e.proto
	}
	if len(names) == 0 {
		c.add(e.m.lineOf("members"), field, "a virtual repository needs at least one member")
		return nil, e.proto
	}
	items := resolveAlias(e.m.value["members"]).Content

	refs := make([]MemberRef, 0, len(names))
	type seen struct {
		name  string
		proto model.Protocol
	}
	var known []seen
	for i, name := range names {
		mfield := fmt.Sprintf("%s[%d]", field, i)
		line := items[i].Line
		e.src.set(fmt.Sprintf("members[%d]", i), line, mfield)
		if _, qualified := qualifiedMemberType(name); qualified {
			// Already explicit, typically a repository in another namespace
			// that this file cannot see. The reconciler checks it exists.
			refs = append(refs, MemberRef{Ref: name})
			continue
		}
		target, ok := byName[name]
		if !ok {
			c.add(line, mfield,
				"%q is not declared in this file; a member outside it needs its type, for example hosted/org/%s or hosted/<team>/%s",
				name, name, name)
			continue
		}
		if target.failed {
			continue
		}
		refs = append(refs, MemberRef{Ref: string(target.typ) + "/" + name})
		if target.proto != "" {
			known = append(known, seen{name: name, proto: target.proto})
		}
	}

	proto := e.proto
	for _, k := range known {
		if proto == "" {
			proto = k.proto
			continue
		}
		if k.proto != proto {
			parts := make([]string, 0, len(known))
			for _, kk := range known {
				parts = append(parts, fmt.Sprintf("%s is %s", kk.name, kk.proto))
			}
			if e.proto != "" {
				parts = append([]string{fmt.Sprintf("%s declares %s", e.name, e.proto)}, parts...)
			}
			c.add(e.m.lineOf("members"), field,
				"members disagree on protocol (%s); one URL serves one wire protocol, so every member of a virtual repository must share one",
				strings.Join(parts, ", "))
			return refs, proto
		}
	}
	if proto == "" {
		c.add(e.m.lineOf("members"), e.src.Path+".protocol",
			"cannot be inferred because no member is declared in this file; write protocol: oci or pypi")
	}
	return refs, proto
}
