package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

var (
	// ErrLocked means a manifest tries to change an organisation repository
	// that the OrgPolicy locks. Teams reference a locked repository; they
	// never override it.
	ErrLocked = errors.New("repository is locked by the organisation policy")

	// ErrOutOfScope means the caller may apply only some namespaces, and the
	// manifests reach beyond them.
	ErrOutOfScope = errors.New("manifest is outside the caller's namespaces")
)

// Scope is what the caller applying desired state may change. The server
// fills it from the authenticated principal; the reconciler enforces it.
type Scope struct {
	// Global is an administrator: every namespace, the organisation policy,
	// the lifecycle, and locked repositories.
	Global bool
	// Namespaces a namespace administrator may apply. They may change only
	// repositories in these namespaces.
	Namespaces []string
}

// Options controls one reconcile.
type Options struct {
	// Source is the trigger: model.SourceFile for serve --config, or
	// model.SourceApply for sindri apply.
	Source string
	Scope  Scope
	// Prune deletes orphaned repositories instead of only reporting them.
	Prune bool
	// DryRun computes the result and rolls it back. `sindri diff` is a dry
	// run of `sindri apply`, so the two cannot disagree.
	DryRun bool
}

// StageChange reports a change to the lifecycle stages.
type StageChange struct {
	Before []string `json:"before"`
	After  []string `json:"after"`
}

// Result is what a reconcile did, or would do.
type Result struct {
	Repositories registry.Changes `json:"repositories"`
	Stages       *StageChange     `json:"stages,omitempty"`
	OrgPolicy    bool             `json:"orgPolicyChanged"`
	DryRun       bool             `json:"dryRun"`
}

// Drift reports whether anything differs from desired state.
func (r Result) Drift() bool {
	return r.Repositories.Drift() || r.Stages != nil || r.OrgPolicy
}

// Reconciler converges the database onto desired state. There is one, and
// two triggers invoke it: serve --config on boot and SIGHUP, and sindri apply
// through the management API (ADR 7).
type Reconciler struct {
	db          *db.DB
	reg         *registry.Registry
	credentials func() map[string]struct{}
}

// New wires a Reconciler. credentials returns the server's defined
// credentials references; it is a function because SIGHUP can change them.
func New(database *db.DB, reg *registry.Registry, credentials func() map[string]struct{}) *Reconciler {
	if database == nil || reg == nil {
		panic("reconcile.New: nil dependency")
	}
	if credentials == nil {
		credentials = func() map[string]struct{} { return map[string]struct{}{} }
	}
	return &Reconciler{db: database, reg: reg, credentials: credentials}
}

// Reconcile validates m against the database and converges it, all in one
// transaction: stages, policy, and repositories commit together or not at all.
func (r *Reconciler) Reconcile(ctx context.Context, m *config.Manifests, opts Options) (Result, error) {
	res := Result{DryRun: opts.DryRun}
	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("beginning reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.db.Queries().WithTx(tx)

	snap, err := r.reg.Snapshot(ctx, q)
	if err != nil {
		return res, err
	}
	stored, err := r.storedPolicy(ctx, q)
	if err != nil {
		return res, err
	}
	before, err := lifecycle.ListStagesIn(ctx, q)
	if err != nil {
		return res, fmt.Errorf("reading stages: %w", err)
	}

	policy := stored
	if m.OrgPolicy != nil {
		policy = m.OrgPolicy
	}
	if err := checkScope(m, opts.Scope, snap, policy); err != nil {
		return res, err
	}

	inSet := make(map[string]bool, len(m.Repositories))
	for _, repo := range m.Repositories {
		inSet[repo.FullName()] = true
	}
	stageNames := make([]string, 0, len(before))
	for _, st := range before {
		stageNames = append(stageNames, st.Name)
	}
	if err := ValidateSet(m, ValidateOptions{
		Credentials: r.credentials(),
		Lookup: func(name string) (Existing, bool) {
			repo, ok := snap.Repositories[name]
			if !ok {
				return Existing{}, false
			}
			return Existing{Type: repo.Type, Protocol: repo.Protocol, Members: snap.Members[name]}, true
		},
		Stages: stageNames,
		Policy: stored,
	}); err != nil {
		return res, err
	}

	if m.OrgPolicy != nil {
		changed, err := writePolicy(ctx, q, m.OrgPolicy, stored)
		if err != nil {
			return res, err
		}
		res.OrgPolicy = changed
	}
	if m.Lifecycle != nil {
		if err := lifecycle.SetStagesIn(ctx, q, StageSpecs(m.Lifecycle)); err != nil {
			return res, fmt.Errorf("writing stages: %w", err)
		}
		after, err := lifecycle.ListStagesIn(ctx, q)
		if err != nil {
			return res, fmt.Errorf("reading stages: %w", err)
		}
		if b, a := describeStages(before), describeStages(after); !slices.Equal(b, a) {
			res.Stages = &StageChange{Before: b, After: a}
		}
	}

	exists := func(name string) bool {
		_, ok := snap.Repositories[name]
		return ok || inSet[name]
	}
	locked := lockedNames(policy)
	desired := make([]registry.DesiredRepository, 0, len(m.Repositories))
	for _, repo := range m.Repositories {
		desired = append(desired, desiredRepository(repo, exists, locked))
	}
	changes, err := r.reg.ApplyConfig(ctx, q, desired, registry.ApplyOptions{
		Source:     opts.Source,
		Namespaces: scopeNamespaces(m, opts.Source),
		Prune:      opts.Prune,
	})
	if err != nil {
		return res, err
	}
	res.Repositories = changes

	if opts.DryRun {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("committing reconcile: %w", err)
	}
	return res, nil
}

func (r *Reconciler) storedPolicy(ctx context.Context, q *db.Queries) (*config.OrgPolicy, error) {
	raw, err := r.reg.OrgPolicyJSON(ctx, q)
	if err != nil || raw == nil {
		return nil, err
	}
	var p config.OrgPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decoding stored organisation policy: %w", err)
	}
	return &p, nil
}

// StoredPolicy returns the organisation policy last applied, or nil.
func (r *Reconciler) StoredPolicy(ctx context.Context) (*config.OrgPolicy, error) {
	return r.storedPolicy(ctx, r.db.Queries())
}

func writePolicy(ctx context.Context, q *db.Queries, p, stored *config.OrgPolicy) (bool, error) {
	clean := *p
	clean.Source = config.Source{}
	raw, err := json.Marshal(clean)
	if err != nil {
		return false, fmt.Errorf("encoding organisation policy: %w", err)
	}
	if stored != nil {
		prev := *stored
		prev.Source = config.Source{}
		if old, err := json.Marshal(prev); err == nil && string(old) == string(raw) {
			return false, nil
		}
	}
	if err := q.UpsertOrgPolicy(ctx, raw); err != nil {
		return false, fmt.Errorf("writing organisation policy: %w", err)
	}
	return true, nil
}

// checkScope enforces what a namespace administrator may change. A global
// administrator may change anything, locked repositories included; a team
// may change only its own namespaces, and never an organisation repository.
func checkScope(m *config.Manifests, s Scope, snap registry.RepositorySnapshot, policy *config.OrgPolicy) error {
	if s.Global {
		return nil
	}
	locked := lockedNames(policy)
	var problems config.Errors
	for _, repo := range m.Repositories {
		full := repo.FullName()
		if existing, ok := snap.Repositories[full]; (ok && existing.Locked) || (repo.Namespace == "" && locked[full]) {
			problems = append(problems, repo.Source.Errorf("", "%s: %q may be referenced by teams but only the organisation changes it",
				ErrLocked, full))
			continue
		}
		if repo.Namespace == "" || !slices.Contains(s.Namespaces, repo.Namespace) {
			problems = append(problems, repo.Source.Errorf("", "%s: the caller administers %s, not %s",
				ErrOutOfScope, describeNamespaces(s.Namespaces), describeNamespace(repo.Namespace)))
		}
	}
	if m.OrgPolicy != nil {
		problems = append(problems, m.OrgPolicy.Source.Errorf("", "%s: only an administrator applies the OrgPolicy", ErrOutOfScope))
	}
	if m.Lifecycle != nil {
		problems = append(problems, m.Lifecycle.Source.Errorf("", "%s: only an administrator applies the Lifecycle", ErrOutOfScope))
	}
	if len(problems) == 0 {
		return nil
	}
	if strings.Contains(problems.Error(), ErrLocked.Error()) {
		return fmt.Errorf("%w: %w", ErrLocked, problems)
	}
	return fmt.Errorf("%w: %w", ErrOutOfScope, problems)
}

func describeNamespaces(ns []string) string {
	if len(ns) == 0 {
		return "no namespace"
	}
	return "namespace " + strings.Join(ns, ", ")
}

func describeNamespace(ns string) string {
	if ns == "" {
		return "the organisation"
	}
	return "namespace " + ns
}

func lockedNames(p *config.OrgPolicy) map[string]bool {
	out := map[string]bool{}
	if p == nil {
		return out
	}
	for _, mr := range p.MandatoryRepos {
		if mr.Locked {
			out[mr.Name] = true
		}
	}
	return out
}

// scopeNamespaces is where orphans are looked for: the namespaces this
// desired state speaks for. A namespace absent from an apply is not
// emptied by it.
func scopeNamespaces(m *config.Manifests, source string) []string {
	if source == model.SourceFile {
		// sindri.yaml speaks for the organisation, all of it.
		return []string{""}
	}
	seen := map[string]bool{}
	var out []string
	add := func(ns string) {
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	for _, r := range m.Repositories {
		add(r.Namespace)
	}
	if m.OrgPolicy != nil || m.Lifecycle != nil {
		add("")
	}
	if m.OrgPolicy != nil {
		for _, ns := range m.OrgPolicy.Namespaces {
			add(ns.Name)
		}
	}
	return out
}

func desiredRepository(repo config.Repository, exists func(string) bool, locked map[string]bool) registry.DesiredRepository {
	spec := repo.Spec
	d := registry.DesiredRepository{
		Name:      repo.FullName(),
		Namespace: repo.Namespace,
		Type:      spec.Type,
		Protocol:  spec.Protocol,
		Locked:    repo.Namespace == "" && locked[repo.FullName()],
		Access:    model.Access{Read: spec.Access.Read, Write: spec.Access.Write},
	}
	if spec.Lifecycle != nil {
		d.Lifecycle = &model.RepoLifecycle{Stages: append([]string{}, spec.Lifecycle.Stages...)}
	}
	if up := spec.Upstream; up != nil {
		d.UpstreamURL = up.URL
		d.CredentialsRef = up.Credentials
		d.TTL = up.TTL.Duration()
		d.NegativeTTL = up.NegativeTTL.Duration()
	}
	for _, m := range spec.Members {
		_, ref := m.Split()
		d.Members = append(d.Members, ResolveRef(repo.Namespace, ref, exists))
	}
	return d
}

func describeStages(stages []lifecycle.Stage) []string {
	out := make([]string, 0, len(stages))
	for _, st := range stages {
		gates := make([]string, 0, len(st.Gates))
		for _, g := range st.Gates {
			gates = append(gates, string(g.Evidence))
		}
		if len(gates) == 0 {
			out = append(out, st.Name)
			continue
		}
		out = append(out, st.Name+"["+strings.Join(gates, ",")+"]")
	}
	return out
}
