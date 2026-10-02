// Package reconcile validates desired state and converges the database onto
// it.
//
// Validation happens here, at write time, and never on the request path:
// downloads and installs stay a lookup, and a misconfiguration fails in a pull
// request rather than on a developer's machine (ARCHITECTURE.md section 7).
// Invariant I4 lives here too: lifecycle configuration on a proxy or virtual
// repository is rejected before any row is written.
package reconcile

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

var (
	// ErrUnknownCredentials means a repository names a credentials reference
	// that the server's credentials section does not define. It is caught when
	// the repository is written, never at pull time (ADR 15).
	ErrUnknownCredentials = errors.New("unknown credentials reference")

	// ErrInvalid means desired state failed validation. The wrapped
	// config.Errors locate each problem.
	ErrInvalid = errors.New("invalid configuration")
)

// RepositorySpec is the subset of a repository declaration the management
// API validates on create. The reconciler validates whole sets with
// ValidateSet, which applies the same rules.
type RepositorySpec struct {
	Name            string
	Type            model.RepoType
	Protocol        model.Protocol
	UpstreamURL     string
	CredentialsRef  string
	Members         []string
	LifecycleStages []string
}

// ValidateRepository checks a repository created through the management API.
// A proxy or virtual repository that carries lifecycle stages fails (I4), and
// a credentials reference must name an entry of the server's credentials
// section.
func ValidateRepository(spec RepositorySpec, credentials map[string]struct{}) error {
	if spec.CredentialsRef != "" {
		if spec.Type != model.RepoTypeProxy {
			return fmt.Errorf("repository %q: only a proxy repository takes credentials", spec.Name)
		}
		if _, ok := credentials[spec.CredentialsRef]; !ok {
			return fmt.Errorf("%w: repository %q names %q, which the server's credentials section does not define",
				ErrUnknownCredentials, spec.Name, spec.CredentialsRef)
		}
	}
	if len(spec.LifecycleStages) == 0 {
		return nil
	}
	switch spec.Type {
	case model.RepoTypeProxy, model.RepoTypeVirtual:
		return lifecycleOnNonHosted(spec.Type, spec.Name)
	case model.RepoTypeHosted, "":
		return nil
	default:
		return fmt.Errorf("unknown repository type %q", spec.Type)
	}
}

func lifecycleOnNonHosted(t model.RepoType, name string) error {
	return fmt.Errorf("lifecycle stages are not allowed on a %s repository %q (invariant I4: only hosted repositories promote)",
		t, name)
}

// Existing describes a repository already in the database, for resolving
// references that leave the set being validated.
type Existing struct {
	Type     model.RepoType
	Protocol model.Protocol
	Members  []string
}

// ValidateOptions supplies what static validation cannot know by itself.
// Every field is optional; `config check` passes none of them, and the server
// passes all of them.
type ValidateOptions struct {
	// Credentials is the set of defined credentials references. Nil skips the
	// check, which is what `config check` on a manifest directory does: the
	// secrets live on the server, not beside the manifests.
	Credentials map[string]struct{}

	// Lookup finds a repository outside the set, by full name.
	Lookup func(name string) (Existing, bool)

	// Stages are the stage names already defined, used when the set carries
	// no Lifecycle of its own.
	Stages []string

	// Policy is the stored organisation policy, used when the set carries
	// none of its own.
	Policy *config.OrgPolicy
}

// ValidateSet checks a whole desired state and reports every problem it
// finds, each located in the file that caused it.
func ValidateSet(m *config.Manifests, opts ValidateOptions) error {
	v := &validator{m: m, opts: opts, byName: map[string]*config.Repository{}}
	v.run()
	if len(v.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, v.errs)
}

type validator struct {
	m      *config.Manifests
	opts   ValidateOptions
	byName map[string]*config.Repository
	errs   config.Errors
}

func (v *validator) add(e *config.FieldError) { v.errs = append(v.errs, e) }

func (v *validator) run() {
	stages := v.validateLifecycle()

	for i := range v.m.Repositories {
		r := &v.m.Repositories[i]
		full := r.FullName()
		if prev, dup := v.byName[full]; dup {
			v.add(r.Source.Errorf("", "repository %q is declared twice (also at %s:%d)", full, prev.Source.File, prev.Source.Line))
			continue
		}
		v.byName[full] = r
	}

	for i := range v.m.Repositories {
		v.validateRepo(&v.m.Repositories[i], stages)
	}
	v.detectCycles()
	v.validatePolicy()
}

// validateLifecycle checks the stage set and returns the stage names that
// repositories may refer to, or nil when they cannot be known statically.
func (v *validator) validateLifecycle() map[string]struct{} {
	lc := v.m.Lifecycle
	if lc == nil {
		if v.opts.Stages == nil {
			return nil
		}
		out := make(map[string]struct{}, len(v.opts.Stages))
		for _, s := range v.opts.Stages {
			out[s] = struct{}{}
		}
		return out
	}
	specs := StageSpecs(lc)
	for i, spec := range specs {
		if err := lifecycle.ValidateStages([]lifecycle.StageSpec{spec}); err != nil {
			v.add(lc.Source.Errorf(fmt.Sprintf("stages[%d]", i), "%s", trimSentinel(err)))
		}
	}
	out := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if _, dup := out[spec.Name]; dup {
			v.add(lc.Source.Errorf(fmt.Sprintf("stages[%d]", i), "stage %q is declared twice", spec.Name))
		}
		out[spec.Name] = struct{}{}
	}
	return out
}

// StageSpecs converts the Lifecycle document into the lifecycle service's
// input, so the reconciler writes stages through the same SetStages call the
// management API uses (ADR 16).
func StageSpecs(lc *config.Lifecycle) []lifecycle.StageSpec {
	specs := make([]lifecycle.StageSpec, 0, len(lc.Stages))
	for _, st := range lc.Stages {
		spec := lifecycle.StageSpec{Name: st.Name}
		for _, g := range st.Gates {
			spec.Gates = append(spec.Gates, lifecycle.GateRule{Evidence: g})
		}
		specs = append(specs, spec)
	}
	return specs
}

func trimSentinel(err error) string {
	msg := err.Error()
	for _, prefix := range []string{lifecycle.ErrInvalidInput.Error() + ": ", registry.ErrInvalidInput.Error() + ": "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

func (v *validator) validateRepo(r *config.Repository, stages map[string]struct{}) {
	src := r.Source
	full := r.FullName()

	if r.Namespace != "" {
		if err := ValidateNamespace(r.Namespace); err != nil {
			v.add(src.Errorf("", "metadata.namespace: %s", err))
		}
	} else if strings.HasPrefix(full, OrgPrefix) {
		v.add(src.Errorf("", "an organisation repository cannot be named %q: the %q prefix is how references reach organisation repositories", full, OrgPrefix))
	}
	if err := registry.ValidateRepoName(full); err != nil {
		v.add(src.Errorf("", "%s", trimSentinel(err)))
	}

	spec := r.Spec
	switch spec.Protocol {
	case model.ProtocolOCI, model.ProtocolPyPI:
	default:
		v.add(src.Errorf("protocol", "%q is not a protocol this build serves, want oci or pypi", spec.Protocol))
	}

	switch spec.Type {
	case model.RepoTypeHosted:
		if spec.Upstream != nil {
			v.add(src.Errorf("upstream", "a hosted repository takes no upstream"))
		}
		if len(spec.Members) > 0 {
			v.add(src.Errorf("members", "a hosted repository takes no members"))
		}
	case model.RepoTypeProxy:
		if len(spec.Members) > 0 {
			v.add(src.Errorf("members", "a proxy repository takes no members"))
		}
		if spec.Upstream == nil {
			v.add(src.Errorf("upstream", "a proxy repository needs an upstream"))
		} else {
			v.validateUpstream(r)
		}
	case model.RepoTypeVirtual:
		if spec.Upstream != nil {
			v.add(src.Errorf("upstream", "a virtual repository takes no upstream"))
		}
		if len(spec.Members) == 0 {
			v.add(src.Errorf("members", "a virtual repository needs at least one member"))
		}
		v.validateMembers(r)
	default:
		v.add(src.Errorf("type", "%q is not a type, want hosted, proxy, or virtual", spec.Type))
	}

	if spec.Lifecycle != nil {
		if spec.Type != model.RepoTypeHosted {
			v.add(src.Errorf("lifecycle", "%s", lifecycleOnNonHosted(spec.Type, full)))
		} else if stages != nil {
			for _, s := range spec.Lifecycle.Stages {
				if _, ok := stages[s]; !ok {
					v.add(src.Errorf("lifecycle", "stage %q is not defined; the lifecycle declares %s", s, stageList(stages)))
				}
			}
		}
	}

	for _, role := range []struct {
		field string
		list  []string
	}{{"access.read", spec.Access.Read}, {"access.write", spec.Access.Write}} {
		for _, g := range role.list {
			if strings.TrimSpace(g) == "" {
				v.add(src.Errorf(role.field, "a group name must not be empty"))
			}
			if g == "anonymous" || g == "*" {
				v.add(src.Errorf(role.field, "%q is not a grant: v1 has no anonymous access; use %q for every signed-in caller",
					g, config.AccessAuthenticated))
			}
		}
	}
}

func stageList(stages map[string]struct{}) string {
	names := make([]string, 0, len(stages))
	for s := range stages {
		names = append(names, s)
	}
	if len(names) == 0 {
		return "no stages"
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func (v *validator) validateUpstream(r *config.Repository) {
	up := r.Spec.Upstream
	u, err := url.Parse(up.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.add(r.Source.Errorf("upstream.url", "%q is not an http(s) URL", up.URL))
	}
	if up.Credentials != "" && v.opts.Credentials != nil {
		if _, ok := v.opts.Credentials[up.Credentials]; !ok {
			v.add(r.Source.Errorf("upstream.credentials", "%s: %q is not defined in the server's credentials section",
				ErrUnknownCredentials, up.Credentials))
		}
	}
}

// OrgPrefix is how a team's manifest reaches an organisation repository.
const OrgPrefix = "org/"

// ResolveRef turns a member reference written in namespace into a full
// repository name.
//
// A reference without a slash is local: inside a team namespace it names that
// team's repository, never an organisation one. "org/<name>" reaches the
// organisation, and anything else with a slash is first tried locally and
// otherwise taken as a full name, which is how "<team>/<name>" reaches another
// team. Every cross-boundary reference is therefore visibly one.
func ResolveRef(namespace, ref string, exists func(string) bool) string {
	if rest, ok := strings.CutPrefix(ref, OrgPrefix); ok {
		return rest
	}
	if namespace == "" {
		return ref
	}
	local := namespace + "/" + ref
	if !strings.Contains(ref, "/") || exists(local) {
		return local
	}
	return ref
}

func (v *validator) exists(name string) bool {
	if _, ok := v.byName[name]; ok {
		return true
	}
	if v.opts.Lookup != nil {
		_, ok := v.opts.Lookup(name)
		return ok
	}
	return false
}

// target describes a member as the validator can see it.
func (v *validator) target(name string) (Existing, bool) {
	if r, ok := v.byName[name]; ok {
		e := Existing{Type: r.Spec.Type, Protocol: r.Spec.Protocol}
		for _, m := range r.Spec.Members {
			_, ref := m.Split()
			e.Members = append(e.Members, ResolveRef(r.Namespace, ref, v.exists))
		}
		return e, true
	}
	if v.opts.Lookup != nil {
		return v.opts.Lookup(name)
	}
	return Existing{}, false
}

func (v *validator) validateMembers(r *config.Repository) {
	seen := map[string]bool{}
	full := r.FullName()
	for i, m := range r.Spec.Members {
		field := fmt.Sprintf("members[%d]", i)
		declared, ref := m.Split()
		switch declared {
		case model.RepoTypeHosted, model.RepoTypeProxy, model.RepoTypeVirtual:
		default:
			v.add(r.Source.Errorf(field, "ref %q must start with its type: hosted/, proxy/, or virtual/", m.Ref))
			continue
		}
		if ref == "" {
			v.add(r.Source.Errorf(field, "ref %q names no repository", m.Ref))
			continue
		}
		name := ResolveRef(r.Namespace, ref, v.exists)
		if name == full {
			v.add(r.Source.Errorf(field, "a virtual repository cannot contain itself"))
			continue
		}
		if seen[name] {
			v.add(r.Source.Errorf(field, "%q is listed twice", name))
			continue
		}
		seen[name] = true

		t, ok := v.target(name)
		if !ok {
			if v.opts.Lookup == nil && r.Namespace != namespaceOf(name) {
				// Outside this set and no database to ask: the server checks it
				// when the set is applied.
				continue
			}
			v.add(r.Source.Errorf(field, "%q resolves to repository %q, which does not exist", m.Ref, name))
			continue
		}
		if t.Type != declared {
			v.add(r.Source.Errorf(field, "%q says %s, but %q is a %s repository", m.Ref, declared, name, t.Type))
		}
		if r.Spec.Protocol != "" && t.Protocol != r.Spec.Protocol {
			v.add(r.Source.Errorf(field,
				"member %q is %s but %q is %s; one URL serves one wire protocol, so every member of a virtual repository must share one",
				name, t.Protocol, full, r.Spec.Protocol))
		}
	}
}

// namespaceOf returns the namespace a full name would belong to if it were a
// team repository. It is only a guess for the purpose of deciding whether a
// reference left the set.
func namespaceOf(name string) string {
	ns, _, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	return ns
}

// detectCycles rejects virtual repositories that contain themselves through
// other virtual repositories.
func (v *validator) detectCycles() {
	for i := range v.m.Repositories {
		r := &v.m.Repositories[i]
		if r.Spec.Type != model.RepoTypeVirtual {
			continue
		}
		start := r.FullName()
		visited := map[string]bool{}
		var walk func(name string, depth int) bool
		walk = func(name string, depth int) bool {
			if depth > 64 {
				return true
			}
			t, ok := v.target(name)
			if !ok || t.Type != model.RepoTypeVirtual {
				return false
			}
			for _, child := range t.Members {
				if child == start {
					return true
				}
				if visited[child] {
					continue
				}
				visited[child] = true
				if walk(child, depth+1) {
					return true
				}
			}
			return false
		}
		if walk(start, 0) {
			v.add(r.Source.Errorf("members", "virtual repository %q contains itself through its members", start))
		}
	}
}

func (v *validator) validatePolicy() {
	p := v.m.OrgPolicy
	if p == nil {
		p = v.opts.Policy
	}
	if p == nil {
		return
	}
	src := p.Source
	if p.Mode == config.PolicyCentralized {
		for i := range v.m.Repositories {
			r := &v.m.Repositories[i]
			if r.Namespace != "" {
				v.add(r.Source.Errorf("", "the organisation policy is centralized: team namespaces cannot declare repositories"))
			}
		}
	}
	for i, ns := range p.Namespaces {
		if err := ValidateNamespace(ns.Name); err != nil {
			v.add(src.Errorf(fmt.Sprintf("namespaces[%d]", i), "%s", err))
		}
	}
	for i, mr := range p.MandatoryRepos {
		field := fmt.Sprintf("mandatoryRepos[%d]", i)
		if strings.Contains(mr.Name, "/") && strings.HasPrefix(mr.Name, OrgPrefix) {
			v.add(src.Errorf(field, "mandatory repositories are organisation repositories; write %q without the %q prefix",
				strings.TrimPrefix(mr.Name, OrgPrefix), OrgPrefix))
			continue
		}
		if p.Mode == config.PolicyDelegated {
			continue
		}
		if !v.exists(mr.Name) {
			v.add(src.Errorf(field, "mandatory repository %q is not declared", mr.Name))
		}
	}
}

// ValidateNamespace checks a team namespace name. It becomes the first segment
// of every repository the team declares, so it follows the stage-name rule
// and must not be "org", which references reserve for the organisation.
func ValidateNamespace(ns string) error {
	if ns == "" || len(ns) > 63 {
		return fmt.Errorf("namespace %q must be 1 to 63 characters", ns)
	}
	if ns+"/" == OrgPrefix {
		return fmt.Errorf("namespace %q is reserved for references to organisation repositories", ns)
	}
	for i, c := range ns {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return fmt.Errorf("namespace %q must be lowercase letters, digits, and '-'", ns)
		}
	}
	return registry.ValidateRepoName(ns)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
