//go:build integration

package reconcile_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

type recEnv struct {
	database *db.DB
	reg      *registry.Registry
	rec      *reconcile.Reconciler
}

func newRecEnv(t *testing.T, credentials ...string) *recEnv {
	t.Helper()
	url := testsupport.StartPostgres(t)
	opts := testsupport.DBOptions(url)
	if err := db.Migrate(t.Context(), opts, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	database, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(database.Close)
	reg := registry.New(database, cas.New(memstore.New()), "memory")
	known := map[string]struct{}{}
	for _, c := range credentials {
		known[c] = struct{}{}
	}
	return &recEnv{
		database: database,
		reg:      reg,
		rec:      reconcile.New(database, reg, func() map[string]struct{} { return known }),
	}
}

func (e *recEnv) apply(t *testing.T, m *config.Manifests, opts reconcile.Options) reconcile.Result {
	t.Helper()
	if opts.Source == "" {
		opts.Source = model.SourceApply
	}
	if !opts.Scope.Global && opts.Scope.Namespaces == nil {
		opts.Scope.Global = true
	}
	res, err := e.rec.Reconcile(t.Context(), m, opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// fixtureCredentials is the credentials reference the gate fixtures use, so
// an environment that reconciles them must define it.
const fixtureCredentials = "mirror"

func loadShort(t *testing.T) *config.Manifests {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Desired
}

func loadLong(t *testing.T) *config.Manifests {
	t.Helper()
	m, err := config.LoadManifests(filepath.Join("..", "..", "testdata", "manifests"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func parse(t *testing.T, docs string) *config.Manifests {
	t.Helper()
	m, err := config.ParseManifests([]byte(docs), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Gate test 1, the desugaring guarantee made executable against a database:
// the short-form sindri.yaml, reconciled the way `serve --config` does, and
// the equivalent manifest directory, reconciled the way `sindri apply` does,
// leave identical database state. The only column allowed to differ is the
// trigger that owns the rows, which records how the state arrived rather than
// what it is.
//
// If this fails, the two spellings have drifted into two systems. Fix the
// desugaring; never relax this comparison.
func TestReconcile_ShortAndLongFormsProduceIdenticalDatabaseState(t *testing.T) {
	short, long := newRecEnv(t, fixtureCredentials), newRecEnv(t, fixtureCredentials)
	short.apply(t, loadShort(t), reconcile.Options{Source: model.SourceFile})
	long.apply(t, loadLong(t), reconcile.Options{Source: model.SourceApply})

	a, b := dumpState(t, short.database), dumpState(t, long.database)
	if len(a) == 0 {
		t.Fatal("the reconcile wrote nothing; the comparison would be vacuous")
	}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Fatalf("short and long forms produced different databases (-short +long):\n%s", diff)
	}
}

// dumpState renders every row the reconciler owns, without ids or
// timestamps, in a stable order.
func dumpState(t *testing.T, database *db.DB) []string {
	t.Helper()
	ctx := t.Context()
	var out []string
	query := func(label, sql string) {
		rows, err := database.Pool().Query(ctx, sql)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			b, err := json.Marshal(vals)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, label+" "+string(b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	query("repository", `SELECT name, type, protocol, config - 'source', locked, managed_by FROM repositories ORDER BY name`)
	query("member", `SELECT v.name, r.name, m.position FROM repository_members m
		JOIN repositories v ON v.id = m.virtual_repo_id JOIN repositories r ON r.id = m.member_repo_id
		ORDER BY v.name, m.position`)
	query("remote", `SELECT r.name, upstream_url, credentials_ref, ttl_seconds, negative_ttl_seconds
		FROM remotes JOIN repositories r ON r.id = remotes.repo_id ORDER BY r.name`)
	query("stage", `SELECT name, position FROM lifecycle_stages ORDER BY position`)
	query("gate", `SELECT s.name, g.position, g.rule FROM promotion_gates g
		JOIN lifecycle_stages s ON s.id = g.stage_id ORDER BY s.name, g.position`)
	query("policy", `SELECT spec FROM org_policy`)
	return out
}

// Gate test 4, first half: converging twice is a no-op, and a diff after the
// first apply reports nothing.
func TestReconcile_SecondRunIsNoOp(t *testing.T) {
	env := newRecEnv(t, fixtureCredentials)
	first := env.apply(t, loadLong(t), reconcile.Options{})
	if len(first.Repositories.Created) != 7 || first.Stages == nil {
		t.Fatalf("first apply: %+v", first)
	}
	before := dumpState(t, env.database)

	second := env.apply(t, loadLong(t), reconcile.Options{})
	if second.Drift() {
		t.Fatalf("second apply changed something: %+v", second)
	}
	if got := len(second.Repositories.Unchanged); got != 7 {
		t.Fatalf("unchanged = %d, want 7", got)
	}
	if diff := cmp.Diff(before, dumpState(t, env.database)); diff != "" {
		t.Fatalf("a no-op apply changed the database:\n%s", diff)
	}
}

// Gate test 4, second half: a change made behind the reconciler's back is
// reported by diff, field by field, and diff itself changes nothing.
func TestReconcile_DiffReportsManualDrift(t *testing.T) {
	env := newRecEnv(t, fixtureCredentials)
	env.apply(t, loadLong(t), reconcile.Options{})

	ctx := t.Context()
	exec := func(sql string) {
		if _, err := env.database.Pool().Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`UPDATE repositories SET config = jsonb_set(config, '{access,write}', '["team:intruder"]') WHERE name = 'internal-py'`)
	exec(`UPDATE remotes SET ttl_seconds = 1 WHERE repo_id = (SELECT id FROM repositories WHERE name = 'pypi')`)
	// Swap python's member order in three steps: the position index is not
	// deferrable.
	for _, step := range [][2]int{{0, 9}, {1, 0}, {9, 1}} {
		exec(fmt.Sprintf(`UPDATE repository_members SET position = %d WHERE position = %d
			AND virtual_repo_id = (SELECT id FROM repositories WHERE name = 'python')`, step[1], step[0]))
	}
	drifted := dumpState(t, env.database)

	diff := env.apply(t, loadLong(t), reconcile.Options{DryRun: true})
	got := map[string]string{}
	for _, u := range diff.Repositories.Updated {
		for _, f := range u.Fields {
			got[u.Name+" "+f.Field] = f.Before + " -> " + f.After
		}
	}
	want := map[string]string{
		"internal-py access.write": "team:intruder -> team:platform",
		"pypi upstream.ttl":        "1s -> 30m0s",
		"python members":           "pypi,internal-py -> internal-py,pypi",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("reported drift (-want +got):\n%s", d)
	}
	if d := cmp.Diff(drifted, dumpState(t, env.database)); d != "" {
		t.Fatalf("diff changed the database:\n%s", d)
	}

	env.apply(t, loadLong(t), reconcile.Options{})
	if again := env.apply(t, loadLong(t), reconcile.Options{DryRun: true}); again.Drift() {
		t.Fatalf("apply did not repair the drift: %+v", again)
	}
}

const orgPolicyDocs = `
kind: OrgPolicy
spec:
  mode: hybrid
  mandatoryRepos:
    - name: approved-base-images
      locked: true
  namespaces:
    - name: payments
      admins: [team:payments-admins]
---
kind: Repository
metadata: { name: approved-base-images }
spec:
  type: hosted
  protocol: oci
  access: { read: [authenticated], write: [team:platform] }
`

// Gate test 5: a team manifest that overrides a locked organisation
// repository is rejected, and nothing it carried is written. The same team
// may reference the locked repository from its own namespace.
func TestReconcile_TeamCannotOverrideLockedOrgRepository(t *testing.T) {
	env := newRecEnv(t)
	env.apply(t, parse(t, orgPolicyDocs), reconcile.Options{})

	repo, err := env.reg.GetRepository(t.Context(), "approved-base-images")
	if err != nil || !repo.Locked {
		t.Fatalf("mandatory repository not locked: %+v %v", repo, err)
	}
	before := dumpState(t, env.database)

	override := parse(t, `
kind: Repository
metadata: { name: approved-base-images }
spec:
  type: hosted
  protocol: oci
  access: { read: [authenticated], write: [team:payments] }
---
kind: Repository
metadata: { name: internal, namespace: payments }
spec: { type: hosted, protocol: oci }
`)
	team := reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Namespaces: []string{"payments"}}}
	_, err = env.rec.Reconcile(t.Context(), override, team)
	if !errors.Is(err, reconcile.ErrLocked) {
		t.Fatalf("override of a locked repository: want ErrLocked, got %v", err)
	}
	if !strings.Contains(err.Error(), "approved-base-images") {
		t.Fatalf("error does not name the locked repository: %v", err)
	}
	if d := cmp.Diff(before, dumpState(t, env.database)); d != "" {
		t.Fatalf("a rejected apply wrote something:\n%s", d)
	}

	// Referencing it is what locked repositories are for.
	ok := parse(t, `
kind: Repository
metadata: { name: internal, namespace: payments }
spec: { type: hosted, protocol: oci }
---
kind: Repository
metadata: { name: images, namespace: payments }
spec:
  type: virtual
  protocol: oci
  members:
    - ref: hosted/internal
    - ref: hosted/org/approved-base-images
`)
	res, err := env.rec.Reconcile(t.Context(), ok, team)
	if err != nil {
		t.Fatalf("team apply in its own namespace: %v", err)
	}
	if !slices.Equal(res.Repositories.Created, []string{"payments/images", "payments/internal"}) {
		t.Fatalf("created %v", res.Repositories.Created)
	}
	members, err := env.reg.ListMembers(t.Context(), "payments/images")
	if err != nil || len(members) != 2 || members[1].Name != "approved-base-images" {
		t.Fatalf("members = %+v, %v", members, err)
	}

	// A team cannot reach outside its namespaces either.
	other := parse(t, "kind: Repository\nmetadata: { name: x, namespace: search }\nspec: { type: hosted, protocol: oci }\n")
	if _, err := env.rec.Reconcile(t.Context(), other, team); !errors.Is(err, reconcile.ErrOutOfScope) {
		t.Fatalf("apply into another namespace: want ErrOutOfScope, got %v", err)
	}
}

// Gate test 6: lifecycle configuration on a proxy repository is rejected at
// reconcile time (invariant I4), and the rejected set writes nothing.
func TestReconcile_RejectsLifecycleOnProxy(t *testing.T) {
	env := newRecEnv(t)
	m := parse(t, `
kind: Lifecycle
spec: { stages: [{ name: prod }] }
---
kind: Repository
metadata: { name: pypi }
spec:
  type: proxy
  protocol: pypi
  upstream: { url: https://pypi.org/simple, ttl: 1h, negativeTtl: 5m }
  lifecycle: { stages: [prod] }
`)
	_, err := env.rec.Reconcile(t.Context(), m, reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Global: true}})
	if !errors.Is(err, reconcile.ErrInvalid) || !strings.Contains(err.Error(), "I4") {
		t.Fatalf("want an I4 rejection, got %v", err)
	}
	if state := dumpState(t, env.database); len(state) != 0 {
		t.Fatalf("a rejected reconcile wrote rows: %v", state)
	}
}

// ADR 15: an unknown credentials reference fails the reconcile, and a known
// one is stored as a reference only.
func TestReconcile_CredentialsReferenceMustExist(t *testing.T) {
	docs := `
kind: Repository
metadata: { name: mirror }
spec:
  type: proxy
  protocol: oci
  upstream: { url: https://registry.example.com, ttl: 1h, negativeTtl: 5m, credentials: fancysecret }
`
	env := newRecEnv(t, "othersecret")
	if _, err := env.rec.Reconcile(t.Context(), parse(t, docs), reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Global: true}}); !errors.Is(err, reconcile.ErrInvalid) || !strings.Contains(err.Error(), "fancysecret") {
		t.Fatalf("unknown reference: %v", err)
	}

	env = newRecEnv(t, "fancysecret")
	env.apply(t, parse(t, docs), reconcile.Options{})
	remote, err := env.reg.GetRemote(t.Context(), "mirror")
	if err != nil || remote.CredentialsRef != "fancysecret" {
		t.Fatalf("remote = %+v, %v", remote, err)
	}
}

// The reconciler and the management API never fight over a row, and neither
// do the two reconciler triggers.
func TestReconcile_RefusesRowsItDoesNotOwn(t *testing.T) {
	env := newRecEnv(t, fixtureCredentials)
	if _, err := env.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: "docker", Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := env.rec.Reconcile(t.Context(), loadLong(t), reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Global: true}})
	if !errors.Is(err, registry.ErrManagedElsewhere) || !strings.Contains(err.Error(), "management API") {
		t.Fatalf("API-managed row: %v", err)
	}
	if err := env.reg.DeleteRepository(t.Context(), "docker"); err != nil {
		t.Fatal(err)
	}

	env.apply(t, loadShort(t), reconcile.Options{Source: model.SourceFile})
	_, err = env.rec.Reconcile(t.Context(), loadLong(t), reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Global: true}})
	if !errors.Is(err, registry.ErrManagedElsewhere) || !strings.Contains(err.Error(), "serve --config") {
		t.Fatalf("row owned by the other trigger: %v", err)
	}
	if err := env.reg.DeleteRepository(t.Context(), "docker"); !errors.Is(err, registry.ErrConfigManaged) {
		t.Fatalf("API delete of a config-managed repository: %v", err)
	}
}

func TestReconcile_TypeChangeIsRefused(t *testing.T) {
	env := newRecEnv(t)
	env.apply(t, parse(t, "kind: Repository\nmetadata: { name: r }\nspec: { type: hosted, protocol: oci }\n"), reconcile.Options{})
	_, err := env.rec.Reconcile(t.Context(),
		parse(t, "kind: Repository\nmetadata: { name: r }\nspec: { type: hosted, protocol: pypi }\n"),
		reconcile.Options{Source: model.SourceApply, Scope: reconcile.Scope{Global: true}})
	if !errors.Is(err, registry.ErrImmutableField) {
		t.Fatalf("protocol change: %v", err)
	}
}

// Removing a repository from the manifests reports it; only --prune deletes.
func TestReconcile_PruneIsOptIn(t *testing.T) {
	env := newRecEnv(t)
	two := "kind: Repository\nmetadata: { name: a }\nspec: { type: hosted, protocol: oci }\n---\n" +
		"kind: Repository\nmetadata: { name: b }\nspec: { type: hosted, protocol: oci }\n"
	env.apply(t, parse(t, two), reconcile.Options{})

	one := parse(t, "kind: Repository\nmetadata: { name: a }\nspec: { type: hosted, protocol: oci }\n")
	res := env.apply(t, one, reconcile.Options{})
	if !slices.Equal(res.Repositories.Orphaned, []string{"b"}) || len(res.Repositories.Pruned) != 0 {
		t.Fatalf("without prune: %+v", res.Repositories)
	}
	if _, err := env.reg.GetRepository(t.Context(), "b"); err != nil {
		t.Fatalf("an orphan was deleted without --prune: %v", err)
	}

	res = env.apply(t, one, reconcile.Options{Prune: true})
	if !slices.Equal(res.Repositories.Pruned, []string{"b"}) {
		t.Fatalf("with prune: %+v", res.Repositories)
	}
	if _, err := env.reg.GetRepository(t.Context(), "b"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("pruned repository still exists: %v", err)
	}
}
