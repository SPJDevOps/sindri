// Package lifecycle owns stages, promotion gates, and the append-only ledger
// of promotion and revocation events (invariant I3, ADR 4, ADR 16).
//
// Promotion changes state, never location: every operation here reads or
// appends rows. Nothing in this package touches the CAS, and the integration
// suite asserts the blob count is unchanged across a promotion sequence.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

var (
	// ErrSubjectNotFound means no hosted publish has made this digest a subject.
	ErrSubjectNotFound = errors.New("subject not found")

	// ErrRepositoryNotFound means the named repository does not exist.
	ErrRepositoryNotFound = errors.New("repository not found")

	// ErrUnknownStage means the named stage is not defined.
	ErrUnknownStage = errors.New("unknown lifecycle stage")

	// ErrNotPromotable means the repository is a proxy or virtual repository.
	// Only hosted content is ours to promote (invariant I4).
	ErrNotPromotable = errors.New("only hosted repositories promote")

	// ErrNotPlaced means the subject is not placed in the named repository.
	ErrNotPlaced = errors.New("subject not placed in repository")

	// ErrNotApproved means a revocation names a state that is not currently
	// approved, so there is nothing to revoke.
	ErrNotApproved = errors.New("subject is not approved for this stage")

	// ErrStageInUse means a stage set would remove a stage the ledger refers to.
	ErrStageInUse = errors.New("stage has lifecycle history")

	// ErrInvalidInput means a stage definition or gate rule is malformed.
	ErrInvalidInput = errors.New("invalid input")

	// ErrGateFailed is matched by every *GateError.
	ErrGateFailed = errors.New("promotion gate failed")
)

// GateError reports which gates refused a promotion. Missing lists the
// evidence types a presence rule requires and the subject lacks; Violations
// lists every severity rule that refused it and why.
type GateError struct {
	Stage      string
	Missing    []string
	Violations []GateViolation
}

// GateViolation is one scan rule's refusal. Findings names what is above the
// ceiling for Platform ("" when the subject has one scan or none). Reason
// explains a refusal that has no findings: no Sindri scan, or one written
// before scans listed their vulnerabilities.
type GateViolation struct {
	Rule     string    `json:"rule"`
	Platform string    `json:"platform,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Finding is one vulnerability in a scan: its id and Trivy's severity
// (CRITICAL, HIGH, MEDIUM, LOW, or UNKNOWN).
type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
}

// maxNamedFindings bounds how many CVE ids Error() spells out; the rest are
// counted, and Violations holds all of them.
const maxNamedFindings = 10

func (e *GateError) Error() string {
	if len(e.Violations) == 0 {
		return fmt.Sprintf("promotion to %s requires evidence: %s", e.Stage, strings.Join(e.Missing, ", "))
	}
	parts := make([]string, 0, len(e.Violations)+1)
	if len(e.Missing) > 0 {
		parts = append(parts, "requires evidence: "+strings.Join(e.Missing, ", "))
	}
	named := 0
	for _, v := range e.Violations {
		var b strings.Builder
		b.WriteString(v.Rule)
		if v.Platform != "" {
			b.WriteString(" (" + v.Platform + ")")
		}
		if v.Reason != "" {
			b.WriteString(": " + v.Reason)
		}
		if len(v.Findings) > 0 {
			b.WriteString(": findings above the ceiling: ")
			ids := make([]string, 0, len(v.Findings))
			for _, f := range v.Findings {
				if named == maxNamedFindings {
					break
				}
				ids = append(ids, f.ID+" "+f.Severity)
				named++
			}
			b.WriteString(strings.Join(ids, ", "))
			if more := len(v.Findings) - len(ids); more > 0 {
				fmt.Fprintf(&b, " and %d more", more)
			}
		}
		parts = append(parts, b.String())
	}
	return fmt.Sprintf("promotion to %s refused: %s", e.Stage, strings.Join(parts, "; "))
}

// Is makes errors.Is(err, ErrGateFailed) true for a *GateError.
func (e *GateError) Is(target error) bool { return target == ErrGateFailed }

// Action is what a ledger event did.
type Action string

const (
	// ActionApprove records a promotion.
	ActionApprove Action = "approve"
	// ActionRevoke records a revocation. It never deletes the approval.
	ActionRevoke Action = "revoke"
)

// Service reads and appends lifecycle state.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// New wires a Service onto the shared database.
func New(database *db.DB) *Service {
	if database == nil {
		panic("lifecycle.New: nil database")
	}
	return &Service{pool: database.Pool(), q: database.Queries()}
}

// Event is one row of the ledger.
type Event struct {
	ID         int64
	Digest     model.Digest
	Repository string
	Stage      string
	Action     Action
	Actor      string
	Reason     string
	At         time.Time
	// EvidenceIDs are the evidence rows the stage's gates read when an
	// approve was recorded. Empty for a revoke, an ungated approve, and
	// events older than the column.
	EvidenceIDs []string
}

// ChangeParams names a promotion or revocation.
type ChangeParams struct {
	Digest     model.Digest
	Repository string
	Stage      string
	Actor      string
	Reason     string
}

// Promote appends one approve event after the stage's gates pass. A repeated
// promotion appends again: the ledger records each act, not a status.
func (s *Service) Promote(ctx context.Context, p ChangeParams) (Event, error) {
	return s.change(ctx, p, ActionApprove)
}

// Revoke appends one revoke event for a currently approved state. The
// approval it supersedes stays in the ledger.
func (s *Service) Revoke(ctx context.Context, p ChangeParams) (Event, error) {
	return s.change(ctx, p, ActionRevoke)
}

func (s *Service) change(ctx context.Context, p ChangeParams, action Action) (Event, error) {
	if !p.Digest.Valid() {
		return Event{}, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	if strings.TrimSpace(p.Actor) == "" {
		return Event{}, fmt.Errorf("%w: actor is required", ErrInvalidInput)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("beginning %s transaction: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	repo, stage, err := resolveTarget(ctx, q, p)
	if err != nil {
		return Event{}, err
	}

	var evidenceIDs []uuid.UUID
	switch action {
	case ActionApprove:
		if !repoAllowsStage(repo.Config, stage.Name) {
			return Event{}, fmt.Errorf("%w: %s may promote only into the stages its lifecycle lists, and %s is not one of them",
				ErrNotPromotable, repo.Name, stage.Name)
		}
		evidenceIDs, err = evaluateGates(ctx, q, p.Digest, stage)
		if err != nil {
			return Event{}, err
		}
	case ActionRevoke:
		current, err := q.GetCurrentState(ctx, db.GetCurrentStateParams{
			SubjectDigest: p.Digest.String(),
			StageID:       stage.ID,
			RepoID:        repo.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && current.Action != string(ActionApprove)) {
			return Event{}, fmt.Errorf("%w: %s in %s at %s", ErrNotApproved, p.Digest, p.Repository, p.Stage)
		}
		if err != nil {
			return Event{}, fmt.Errorf("reading current state of %s: %w", p.Digest, err)
		}
	}

	row, err := q.AppendLifecycleEvent(ctx, db.AppendLifecycleEventParams{
		SubjectDigest: p.Digest.String(),
		RepoID:        repo.ID,
		StageID:       stage.ID,
		Action:        string(action),
		Actor:         p.Actor,
		Reason:        p.Reason,
		EvidenceIds:   evidenceIDs,
	})
	if err != nil {
		return Event{}, fmt.Errorf("appending %s event for %s: %w", action, p.Digest, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("committing %s: %w", action, err)
	}

	return Event{
		ID:         row.ID,
		Digest:     p.Digest,
		Repository: repo.Name,
		Stage:      stage.Name,
		Action:     action,
		Actor:      row.Actor,
		Reason:     row.Reason,
		At:         row.At,

		EvidenceIDs: uuidStrings(row.EvidenceIds),
	}, nil
}

// uuidStrings renders ids for an Event; nil stays nil so JSON omits it.
func uuidStrings(ids []uuid.UUID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// repoAllowsStage reads the repository's lifecycle restriction from its config
// column (written by the registry, see registry.repoConfig). Absent means every
// stage. Revocation is never restricted: withdrawing trust must always work.
func repoAllowsStage(cfg []byte, stage string) bool {
	var rc struct {
		LifecycleStages *[]string `json:"lifecycleStages"`
	}
	if err := json.Unmarshal(cfg, &rc); err != nil {
		return false // an unreadable restriction denies, it does not allow
	}
	if rc.LifecycleStages == nil {
		return true
	}
	return (&model.RepoLifecycle{Stages: *rc.LifecycleStages}).AllowsStage(stage)
}

// resolveTarget checks everything a ledger write depends on: the subject
// exists, the repository is hosted (invariant I4 on the promotion path, not
// only at reconcile time), the subject is placed there, and the stage exists.
func resolveTarget(ctx context.Context, q *db.Queries, p ChangeParams) (db.Repository, db.LifecycleStage, error) {
	if _, err := q.GetSubject(ctx, p.Digest.String()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("%w: %s", ErrSubjectNotFound, p.Digest)
		}
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("looking up subject %s: %w", p.Digest, err)
	}

	repo, err := q.GetRepositoryByName(ctx, p.Repository)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("%w: %q", ErrRepositoryNotFound, p.Repository)
		}
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("looking up repository %s: %w", p.Repository, err)
	}
	if repo.Type != string(model.RepoTypeHosted) {
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("%w: %s is %s", ErrNotPromotable, repo.Name, repo.Type)
	}

	placed, err := q.CheckPlacement(ctx, db.CheckPlacementParams{
		RepoName: repo.Name,
		Digest:   p.Digest.String(),
	})
	if err != nil {
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("checking placement of %s in %s: %w", p.Digest, repo.Name, err)
	}
	if !placed {
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("%w: %s in %s", ErrNotPlaced, p.Digest, repo.Name)
	}

	stage, err := q.GetStageByName(ctx, p.Stage)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("%w: %q", ErrUnknownStage, p.Stage)
		}
		return db.Repository{}, db.LifecycleStage{}, fmt.Errorf("looking up stage %s: %w", p.Stage, err)
	}
	return repo, stage, nil
}

// evaluateGates checks every gate of a stage and returns the ids of the
// evidence they read, for the ledger. A refusal is a *GateError naming every
// presence rule that lacks its evidence and every severity rule that failed,
// so one attempt shows everything to fix. Any other failure, including an
// unreadable stored rule or scan metadata, fails closed.
func evaluateGates(ctx context.Context, q *db.Queries, digest model.Digest, stage db.LifecycleStage) ([]uuid.UUID, error) {
	gates, err := q.ListGatesForStage(ctx, stage.ID)
	if err != nil {
		return nil, fmt.Errorf("loading gates for %s: %w", stage.Name, err)
	}
	if len(gates) == 0 {
		return nil, nil
	}
	rules := make([]GateRule, 0, len(gates))
	for _, g := range gates {
		rule, err := ParseGateRule(g.Rule)
		if err != nil {
			// Rules are validated when written; a bad stored rule fails
			// closed rather than letting a promotion through.
			return nil, fmt.Errorf("stored gate %s on %s: %w", g.ID, stage.Name, err)
		}
		rules = append(rules, rule)
	}

	var (
		newest map[string]uuid.UUID // evidence type -> newest row
		scans  []db.ListNewestSindriScansRow
		loaded bool
	)
	for _, rule := range rules {
		if rule.Scan != nil && !loaded {
			scans, err = q.ListNewestSindriScans(ctx, digest.String())
			if err != nil {
				return nil, fmt.Errorf("loading scans of %s: %w", digest, err)
			}
			loaded = true
		}
		if rule.Scan == nil && newest == nil {
			rows, err := q.ListNewestEvidenceByType(ctx, digest.String())
			if err != nil {
				return nil, fmt.Errorf("loading evidence of %s: %w", digest, err)
			}
			newest = make(map[string]uuid.UUID, len(rows))
			for _, r := range rows {
				newest[r.Type] = r.ID
			}
		}
	}

	var (
		ids        []uuid.UUID
		seen       = make(map[uuid.UUID]struct{})
		missing    []string
		violations []GateViolation
	)
	read := func(id uuid.UUID) {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for _, rule := range rules {
		if rule.Scan == nil {
			id, ok := newest[string(rule.Evidence)]
			if !ok {
				missing = append(missing, string(rule.Evidence))
				continue
			}
			read(id)
			continue
		}
		vs, err := evaluateScanRule(rule, digest, scans, read)
		if err != nil {
			return nil, err
		}
		violations = append(violations, vs...)
	}
	if len(missing) > 0 || len(violations) > 0 {
		return nil, &GateError{Stage: stage.Name, Missing: missing, Violations: violations}
	}
	return ids, nil
}

// evaluateScanRule applies a severity ceiling to the newest Sindri scan of
// each platform (ADR 30). read is told every scan evaluated.
func evaluateScanRule(rule GateRule, digest model.Digest, scans []db.ListNewestSindriScansRow, read func(uuid.UUID)) ([]GateViolation, error) {
	name := rule.String()
	if len(scans) == 0 {
		return []GateViolation{{Rule: name, Reason: "no Sindri scan for " + digest.String()}}, nil
	}
	var out []GateViolation
	have := make(map[string]struct{}, len(scans))
	newest := 0 // the newest scan overall; its platforms list is the required set
	for i, scan := range scans {
		if scan.CreatedAt.After(scans[newest].CreatedAt) {
			newest = i
		}
	}
	var required []string
	for i, scan := range scans {
		read(scan.ID)
		have[scan.Platform] = struct{}{}
		var meta struct {
			Vulnerabilities *[]Finding `json:"vulnerabilities"`
			Platforms       []string   `json:"platforms"`
		}
		if err := json.Unmarshal(scan.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("reading metadata of scan %s: %w", scan.ID, err)
		}
		if i == newest {
			required = meta.Platforms
		}
		switch {
		case meta.Vulnerabilities == nil:
			out = append(out, GateViolation{
				Rule: name, Platform: scan.Platform,
				Reason: "the Sindri scan " + scan.ID.String() + " lists no vulnerabilities; it needs a new scan, and the scanning guide says what queues one for each protocol",
			})
			continue
		case scan.Platform != "" && len(meta.Platforms) == 0:
			out = append(out, GateViolation{
				Rule: name, Platform: scan.Platform,
				Reason: "the Sindri scan " + scan.ID.String() + " does not list the platforms of its index; it needs a new scan, and the scanning guide says what queues one for each protocol",
			})
			continue
		}
		var above []Finding
		for _, f := range *meta.Vulnerabilities {
			if rule.Scan.MaxSeverity.Refuses(f.Severity) {
				above = append(above, f)
			}
		}
		if len(above) > 0 {
			out = append(out, GateViolation{Rule: name, Platform: scan.Platform, Findings: above})
		}
	}
	// An index is complete only when every platform its newest scan job
	// listed has a scan: a job that attached some platforms and then failed
	// must not pass on the part it finished.
	for _, p := range required {
		if _, ok := have[p]; !ok {
			label := "platform " + p
			if p == "" {
				label = "the index child without a platform"
			}
			out = append(out, GateViolation{Rule: name, Platform: p, Reason: "scan incomplete: no Sindri scan for " + label})
		}
	}
	return out, nil
}

// Decision answers "is exactly this digest approved for this environment?"
type Decision struct {
	Digest      model.Digest
	Environment string
	Approved    bool
	// Reason is approved, not_approved, or unknown_environment.
	Reason    string
	Approvals []Approval
}

// Approval is one repository's current approval of a subject at a stage.
type Approval struct {
	Repository string
	Actor      string
	Reason     string
	At         time.Time
}

// Decision reasons.
const (
	ReasonApproved           = "approved"
	ReasonNotApproved        = "not_approved"
	ReasonUnknownEnvironment = "unknown_environment"
)

// Decide reads the current-state view. An unknown environment is denied, not
// an error: an admission controller with a typo must fail closed.
func (s *Service) Decide(ctx context.Context, digest model.Digest, environment string) (Decision, error) {
	if !digest.Valid() {
		return Decision{}, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	decision := Decision{Digest: digest, Environment: environment, Reason: ReasonNotApproved}

	stage, err := s.q.GetStageByName(ctx, environment)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			decision.Reason = ReasonUnknownEnvironment
			return decision, nil
		}
		return Decision{}, fmt.Errorf("looking up stage %s: %w", environment, err)
	}

	rows, err := s.q.ListCurrentApprovals(ctx, db.ListCurrentApprovalsParams{
		SubjectDigest: digest.String(),
		StageID:       stage.ID,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("reading approvals of %s at %s: %w", digest, environment, err)
	}
	for _, row := range rows {
		decision.Approvals = append(decision.Approvals, Approval{
			Repository: row.RepoName,
			Actor:      row.Actor,
			Reason:     row.Reason,
			At:         row.At,
		})
	}
	if len(decision.Approvals) > 0 {
		decision.Approved = true
		decision.Reason = ReasonApproved
	}
	return decision, nil
}

// Events returns the full ledger for a subject, oldest first.
func (s *Service) Events(ctx context.Context, digest model.Digest) ([]Event, error) {
	if err := s.requireSubject(ctx, digest); err != nil {
		return nil, err
	}
	rows, err := s.q.ListLifecycleEvents(ctx, digest.String())
	if err != nil {
		return nil, fmt.Errorf("listing events of %s: %w", digest, err)
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			ID:         row.ID,
			Digest:     digest,
			Repository: row.RepoName,
			Stage:      row.StageName,
			Action:     Action(row.Action),
			Actor:      row.Actor,
			Reason:     row.Reason,
			At:         row.At,

			EvidenceIDs: uuidStrings(row.EvidenceIds),
		})
	}
	return events, nil
}

// Subject is a digest's identity: what it is, who owns it, where it came
// from, and its current lifecycle state in each repository and stage.
type Subject struct {
	Digest     model.Digest
	Size       int64
	Kind       model.SubjectKind
	CreatedAt  time.Time
	Owners     []string
	Provenance []Provenance
	States     []State
}

// Provenance is one recorded claim about a subject's origin.
type Provenance struct {
	model.Provenance
	RecordedAt time.Time
}

// State is the latest event for one (repository, stage).
type State struct {
	Repository string
	Stage      string
	Action     Action
	Actor      string
	At         time.Time
}

// GetSubject returns a subject with its owners, provenance, and states.
func (s *Service) GetSubject(ctx context.Context, digest model.Digest) (Subject, error) {
	if !digest.Valid() {
		return Subject{}, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	row, err := s.q.GetSubject(ctx, digest.String())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Subject{}, fmt.Errorf("%w: %s", ErrSubjectNotFound, digest)
		}
		return Subject{}, fmt.Errorf("looking up subject %s: %w", digest, err)
	}
	owners, err := s.q.ListSubjectOwners(ctx, digest.String())
	if err != nil {
		return Subject{}, fmt.Errorf("listing owners of %s: %w", digest, err)
	}
	provRows, err := s.q.ListProvenance(ctx, digest.String())
	if err != nil {
		return Subject{}, fmt.Errorf("listing provenance of %s: %w", digest, err)
	}
	stateRows, err := s.q.ListCurrentStates(ctx, digest.String())
	if err != nil {
		return Subject{}, fmt.Errorf("listing states of %s: %w", digest, err)
	}

	subject := Subject{
		Digest:     digest,
		Size:       row.Size,
		Kind:       model.SubjectKind(row.Kind),
		CreatedAt:  row.CreatedAt,
		Owners:     owners,
		Provenance: make([]Provenance, 0, len(provRows)),
		States:     make([]State, 0, len(stateRows)),
	}
	for _, p := range provRows {
		subject.Provenance = append(subject.Provenance, Provenance{
			Provenance: model.Provenance{
				SourceType: p.SourceType,
				SourceRef:  p.SourceRef.String,
				BuildRef:   p.BuildRef.String,
			},
			RecordedAt: p.RecordedAt,
		})
	}
	for _, st := range stateRows {
		subject.States = append(subject.States, State{
			Repository: st.RepoName,
			Stage:      st.StageName,
			Action:     Action(st.Action),
			Actor:      st.Actor,
			At:         st.At,
		})
	}
	return subject, nil
}

func (s *Service) requireSubject(ctx context.Context, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	if _, err := s.q.GetSubject(ctx, digest.String()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrSubjectNotFound, digest)
		}
		return fmt.Errorf("looking up subject %s: %w", digest, err)
	}
	return nil
}

// marshalRule is the stored form of a gate rule.
func marshalRule(rule GateRule) ([]byte, error) {
	b, err := json.Marshal(rule)
	if err != nil {
		return nil, fmt.Errorf("encoding gate rule: %w", err)
	}
	return b, nil
}
