package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

var (
	// ErrManagedElsewhere means desired state names a repository that another
	// owner manages: the management API, or the other reconciler trigger. The
	// reconciler and the API never fight over a row (ADR 5).
	ErrManagedElsewhere = errors.New("repository is managed elsewhere")

	// ErrImmutableField means desired state changes a repository's type or
	// protocol. Those decide what the stored bytes mean, so changing one is a
	// delete and a re-create, which the reconciler never does implicitly.
	ErrImmutableField = errors.New("field cannot change")

	// ErrConfigManaged means the management API tried to change a repository
	// the reconciler owns. The change belongs in the configuration.
	ErrConfigManaged = errors.New("repository is managed by configuration")
)

// repoConfig is the repositories.config column. It is read on every request
// that authorizes against a repository, so it stays small.
type repoConfig struct {
	Namespace       string       `json:"namespace,omitempty"`
	Source          string       `json:"source,omitempty"`
	Access          model.Access `json:"access"`
	LifecycleStages *[]string    `json:"lifecycleStages,omitempty"`
}

// parseRepoConfig reads the column. A row written before M7 holds "{}", which
// decodes to no grants: such a repository is reachable by administrators only
// until someone grants access, which fails closed.
func parseRepoConfig(raw []byte) struct {
	Namespace string
	Source    string
	Access    model.Access
	Lifecycle *model.RepoLifecycle
} {
	var rc repoConfig
	_ = json.Unmarshal(raw, &rc) // a malformed column grants nothing, which is the safe reading
	out := struct {
		Namespace string
		Source    string
		Access    model.Access
		Lifecycle *model.RepoLifecycle
	}{Namespace: rc.Namespace, Source: rc.Source, Access: rc.Access}
	if rc.LifecycleStages != nil {
		out.Lifecycle = &model.RepoLifecycle{Stages: *rc.LifecycleStages}
	}
	return out
}

func marshalRepoConfig(namespace, source string, access model.Access, lc *model.RepoLifecycle) ([]byte, error) {
	rc := repoConfig{Namespace: namespace, Source: source, Access: normalizeAccess(access)}
	if lc != nil {
		stages := append([]string{}, lc.Stages...)
		rc.LifecycleStages = &stages
	}
	raw, err := json.Marshal(rc)
	if err != nil {
		return nil, fmt.Errorf("encoding repository config: %w", err)
	}
	return raw, nil
}

func normalizeAccess(a model.Access) model.Access {
	if len(a.Read) == 0 {
		a.Read = nil
	}
	if len(a.Write) == 0 {
		a.Write = nil
	}
	return a
}

// DesiredRepository is one repository as the reconciler wants it, with every
// reference already resolved to a full name.
type DesiredRepository struct {
	Name      string
	Namespace string
	Type      model.RepoType
	Protocol  model.Protocol
	Locked    bool
	Access    model.Access
	Lifecycle *model.RepoLifecycle

	// Proxy only.
	UpstreamURL    string
	CredentialsRef string
	TTL            time.Duration
	NegativeTTL    time.Duration

	// Virtual only: ordered full names.
	Members []string
}

// ApplyOptions controls one convergence.
type ApplyOptions struct {
	// Source is the trigger that owns the rows written: model.SourceFile or
	// model.SourceApply.
	Source string
	// Namespaces is the scope of this apply: a config-managed repository of
	// this source in one of these namespaces ("" is the organisation) that the
	// desired state no longer lists is an orphan. Repositories outside the
	// scope are never touched.
	Namespaces []string
	// Prune deletes orphans. Without it they are only reported, so removing a
	// line from a manifest cannot silently delete a repository's content.
	Prune bool
}

// FieldChange is one drifted field of a repository.
type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// RepositoryChange describes how one repository differs from desired state.
type RepositoryChange struct {
	Name   string        `json:"name"`
	Fields []FieldChange `json:"fields"`
}

// Changes is what a convergence did, or would do.
type Changes struct {
	Created   []string           `json:"created"`
	Updated   []RepositoryChange `json:"updated"`
	Unchanged []string           `json:"unchanged"`
	// Orphaned are config-managed repositories in scope that desired state no
	// longer lists. They are deleted only with Prune; Pruned lists those.
	Orphaned []string `json:"orphaned"`
	Pruned   []string `json:"pruned"`
}

// Drift reports whether applying would change anything, orphans included.
func (c Changes) Drift() bool {
	return len(c.Created) > 0 || len(c.Updated) > 0 || len(c.Orphaned) > 0
}

// ApplyConfig converges config-managed repositories onto desired, using q,
// which the caller binds to a transaction so that stages, policy, and
// repositories commit or fail together. It refuses, rather than overrides, a
// repository another owner manages.
func (r *Registry) ApplyConfig(ctx context.Context, q *db.Queries, desired []DesiredRepository, opts ApplyOptions) (Changes, error) {
	if opts.Source != model.SourceFile && opts.Source != model.SourceApply {
		return Changes{}, fmt.Errorf("%w: unknown config source %q", ErrInvalidInput, opts.Source)
	}
	ch := Changes{Created: []string{}, Updated: []RepositoryChange{}, Unchanged: []string{}, Orphaned: []string{}, Pruned: []string{}}

	rows, err := q.ListAllRepositories(ctx)
	if err != nil {
		return ch, fmt.Errorf("listing repositories: %w", err)
	}
	existing := make(map[string]db.Repository, len(rows))
	for _, row := range rows {
		existing[row.Name] = row
	}

	// Refuse the whole convergence before writing anything if any row
	// belongs to someone else.
	for _, d := range desired {
		row, ok := existing[d.Name]
		if !ok {
			continue
		}
		if err := ownedBy(row, opts.Source); err != nil {
			return ch, err
		}
		if row.Type != string(d.Type) {
			return ch, fmt.Errorf("%w: repository %q is %s and the configuration says %s; delete it and re-create it to change its type",
				ErrImmutableField, d.Name, row.Type, d.Type)
		}
		if row.Protocol != string(d.Protocol) {
			return ch, fmt.Errorf("%w: repository %q speaks %s and the configuration says %s; delete it and re-create it to change its protocol",
				ErrImmutableField, d.Name, row.Protocol, d.Protocol)
		}
	}

	// Rows first, so that every member exists before any membership is
	// written, whatever order the virtual repositories reference each other in.
	ids := make(map[string]uuid.UUID, len(existing)+len(desired))
	for name, row := range existing {
		ids[name] = row.ID
	}
	for _, d := range desired {
		cfg, err := marshalRepoConfig(d.Namespace, opts.Source, d.Access, d.Lifecycle)
		if err != nil {
			return ch, err
		}
		row, ok := existing[d.Name]
		if !ok {
			created, err := q.CreateRepository(ctx, db.CreateRepositoryParams{
				Name:      d.Name,
				Type:      string(d.Type),
				Protocol:  string(d.Protocol),
				Config:    cfg,
				Locked:    d.Locked,
				ManagedBy: string(model.ManagedByConfig),
			})
			if err != nil {
				return ch, fmt.Errorf("creating repository %s: %w", d.Name, err)
			}
			ids[d.Name] = created.ID
			ch.Created = append(ch.Created, d.Name)
			continue
		}
		var fields []FieldChange
		before := parseRepoConfig(row.Config)
		fields = appendIfChanged(fields, "locked", fmt.Sprint(row.Locked), fmt.Sprint(d.Locked))
		fields = appendIfChanged(fields, "namespace", before.Namespace, d.Namespace)
		fields = appendIfChanged(fields, "access.read", strings.Join(before.Access.Read, ","), strings.Join(d.Access.Read, ","))
		fields = appendIfChanged(fields, "access.write", strings.Join(before.Access.Write, ","), strings.Join(d.Access.Write, ","))
		fields = appendIfChanged(fields, "lifecycle.stages", describeLifecycle(before.Lifecycle), describeLifecycle(d.Lifecycle))
		if !bytes.Equal(canonicalJSON(row.Config), canonicalJSON(cfg)) || row.Locked != d.Locked {
			if _, err := q.UpdateRepositoryConfig(ctx, db.UpdateRepositoryConfigParams{ID: row.ID, Config: cfg, Locked: d.Locked}); err != nil {
				return ch, fmt.Errorf("updating repository %s: %w", d.Name, err)
			}
		}
		ch.Updated = append(ch.Updated, RepositoryChange{Name: d.Name, Fields: fields})
	}

	for _, d := range desired {
		id := ids[d.Name]
		var fields []FieldChange
		switch d.Type {
		case model.RepoTypeProxy:
			f, err := convergeRemote(ctx, q, id, d)
			if err != nil {
				return ch, err
			}
			fields = f
		case model.RepoTypeVirtual:
			f, err := convergeMembers(ctx, q, id, ids, d)
			if err != nil {
				return ch, err
			}
			fields = f
		}
		ch.recordFields(d.Name, fields)
	}

	// Anything updated with no field changes is unchanged.
	updated := ch.Updated[:0]
	for _, u := range ch.Updated {
		if len(u.Fields) == 0 {
			ch.Unchanged = append(ch.Unchanged, u.Name)
			continue
		}
		updated = append(updated, u)
	}
	ch.Updated = updated

	want := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		want[d.Name] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := want[row.Name]; ok || row.ManagedBy != string(model.ManagedByConfig) {
			continue
		}
		rc := parseRepoConfig(row.Config)
		if rc.Source != opts.Source || !slices.Contains(opts.Namespaces, rc.Namespace) {
			continue
		}
		ch.Orphaned = append(ch.Orphaned, row.Name)
		if !opts.Prune {
			continue
		}
		if _, err := q.DeleteRepositoryByName(ctx, row.Name); err != nil {
			if isForeignKeyViolation(err, "lifecycle_events_repo_id_fkey") {
				return ch, fmt.Errorf("pruning %s: %w: the ledger refers to it, so it stays", row.Name, ErrHasHistory)
			}
			return ch, fmt.Errorf("pruning %s: %w", row.Name, err)
		}
		ch.Pruned = append(ch.Pruned, row.Name)
	}
	slices.Sort(ch.Created)
	slices.Sort(ch.Unchanged)
	slices.Sort(ch.Orphaned)
	slices.Sort(ch.Pruned)
	slices.SortFunc(ch.Updated, func(a, b RepositoryChange) int { return strings.Compare(a.Name, b.Name) })
	return ch, nil
}

func (c *Changes) recordFields(name string, fields []FieldChange) {
	if len(fields) == 0 {
		return
	}
	for i := range c.Updated {
		if c.Updated[i].Name == name {
			c.Updated[i].Fields = append(c.Updated[i].Fields, fields...)
			return
		}
	}
	// A created repository's remote and members are part of its creation,
	// not drift.
}

// ownedBy refuses a row that the given reconciler source does not own.
func ownedBy(row db.Repository, source string) error {
	if row.ManagedBy != string(model.ManagedByConfig) {
		return fmt.Errorf("%w: repository %q was created through the management API; delete it there, or rename it in the configuration",
			ErrManagedElsewhere, row.Name)
	}
	rc := parseRepoConfig(row.Config)
	if rc.Source != "" && rc.Source != source {
		return fmt.Errorf("%w: repository %q is managed by %s, not by %s",
			ErrManagedElsewhere, row.Name, describeSource(rc.Source), describeSource(source))
	}
	return nil
}

func describeSource(s string) string {
	switch s {
	case model.SourceFile:
		return "serve --config"
	case model.SourceApply:
		return "sindri apply"
	default:
		return s
	}
}

func convergeRemote(ctx context.Context, q *db.Queries, id uuid.UUID, d DesiredRepository) ([]FieldChange, error) {
	url := strings.TrimRight(d.UpstreamURL, "/")
	cred := pgtype.Text{}
	if d.CredentialsRef != "" {
		cred = pgtype.Text{String: d.CredentialsRef, Valid: true}
	}
	ttl, neg := durationSeconds(d.TTL), durationSeconds(d.NegativeTTL)

	var fields []FieldChange
	current, err := q.GetRemote(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		fields = append(fields, FieldChange{Field: "upstream", Before: "", After: url})
	case err != nil:
		return nil, fmt.Errorf("reading remote of %s: %w", d.Name, err)
	default:
		fields = appendIfChanged(fields, "upstream.url", current.UpstreamUrl, url)
		fields = appendIfChanged(fields, "upstream.credentials", current.CredentialsRef.String, cred.String)
		fields = appendIfChanged(fields, "upstream.ttl", secondsString(current.TtlSeconds), secondsString(ttl))
		fields = appendIfChanged(fields, "upstream.negativeTtl", secondsString(current.NegativeTtlSeconds), secondsString(neg))
		if len(fields) == 0 {
			return nil, nil
		}
	}
	if _, err := q.UpsertRemote(ctx, db.UpsertRemoteParams{
		RepoID:             id,
		UpstreamUrl:        url,
		CredentialsRef:     cred,
		TtlSeconds:         ttl,
		NegativeTtlSeconds: neg,
	}); err != nil {
		return nil, fmt.Errorf("writing remote of %s: %w", d.Name, err)
	}
	return fields, nil
}

func convergeMembers(ctx context.Context, q *db.Queries, id uuid.UUID, ids map[string]uuid.UUID, d DesiredRepository) ([]FieldChange, error) {
	current, err := q.ListRepositoryMembers(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading members of %s: %w", d.Name, err)
	}
	have := make([]string, 0, len(current))
	for _, m := range current {
		have = append(have, m.Name)
	}
	if slices.Equal(have, d.Members) {
		return nil, nil
	}
	// The position index is not deferrable, so a reorder is a replace.
	if err := q.DeleteRepositoryMembers(ctx, id); err != nil {
		return nil, fmt.Errorf("clearing members of %s: %w", d.Name, err)
	}
	for i, name := range d.Members {
		memberID, ok := ids[name]
		if !ok {
			return nil, fmt.Errorf("%w: member %q of %s does not exist", ErrInvalidInput, name, d.Name)
		}
		if _, err := q.InsertRepositoryMember(ctx, db.InsertRepositoryMemberParams{
			VirtualRepoID: id,
			MemberRepoID:  memberID,
			Position:      int32(i), //nolint:gosec // G115: member lists are bounded by manifest size
		}); err != nil {
			return nil, fmt.Errorf("recording member %s of %s: %w", name, d.Name, err)
		}
	}
	if len(current) == 0 {
		return nil, nil // a new virtual repository; its creation is the change
	}
	return []FieldChange{{Field: "members", Before: strings.Join(have, ","), After: strings.Join(d.Members, ",")}}, nil
}

func appendIfChanged(fields []FieldChange, field, before, after string) []FieldChange {
	if before == after {
		return fields
	}
	return append(fields, FieldChange{Field: field, Before: before, After: after})
}

func describeLifecycle(l *model.RepoLifecycle) string {
	if l == nil {
		return "(all stages)"
	}
	return "[" + strings.Join(l.Stages, ",") + "]"
}

func durationSeconds(d time.Duration) int32 {
	s := d / time.Second
	if s > 1<<31-1 {
		s = 1<<31 - 1
	}
	return int32(s) //nolint:gosec // G115: clamped to int32 above
}

func secondsString(s int32) string {
	return (time.Duration(s) * time.Second).String()
}

// canonicalJSON re-encodes a jsonb value so that key order and spacing from
// Postgres do not register as drift.
func canonicalJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// OrgPolicyJSON reads the stored organisation policy; nil when none was ever
// applied.
func (r *Registry) OrgPolicyJSON(ctx context.Context, q *db.Queries) ([]byte, error) {
	if q == nil {
		q = r.q
	}
	spec, err := q.GetOrgPolicy(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading organisation policy: %w", err)
	}
	return spec, nil
}

// RepositorySnapshot is every repository with its members, for validating
// references that leave a desired-state set.
type RepositorySnapshot struct {
	Repositories map[string]model.Repository
	Members      map[string][]string
}

// Snapshot reads all repositories and memberships through q.
func (r *Registry) Snapshot(ctx context.Context, q *db.Queries) (RepositorySnapshot, error) {
	if q == nil {
		q = r.q
	}
	rows, err := q.ListAllRepositories(ctx)
	if err != nil {
		return RepositorySnapshot{}, fmt.Errorf("listing repositories: %w", err)
	}
	snap := RepositorySnapshot{Repositories: make(map[string]model.Repository, len(rows)), Members: map[string][]string{}}
	for _, row := range rows {
		snap.Repositories[row.Name] = repositoryFromRow(row)
	}
	members, err := q.ListAllRepositoryMembers(ctx)
	if err != nil {
		return RepositorySnapshot{}, fmt.Errorf("listing members: %w", err)
	}
	for _, m := range members {
		snap.Members[m.VirtualName] = append(snap.Members[m.VirtualName], m.MemberName)
	}
	return snap, nil
}
