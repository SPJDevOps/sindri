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
// evidence types the stage requires and the subject lacks.
type GateError struct {
	Stage   string
	Missing []string
}

func (e *GateError) Error() string {
	return fmt.Sprintf("promotion to %s requires evidence: %s", e.Stage, strings.Join(e.Missing, ", "))
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

	switch action {
	case ActionApprove:
		if err := evaluateGates(ctx, q, p.Digest, stage); err != nil {
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
	}, nil
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

// evaluateGates returns a *GateError naming every required evidence type the
// subject lacks, in the order the stage declares them.
func evaluateGates(ctx context.Context, q *db.Queries, digest model.Digest, stage db.LifecycleStage) error {
	gates, err := q.ListGatesForStage(ctx, stage.ID)
	if err != nil {
		return fmt.Errorf("loading gates for %s: %w", stage.Name, err)
	}
	if len(gates) == 0 {
		return nil
	}
	present, err := q.ListEvidenceTypes(ctx, digest.String())
	if err != nil {
		return fmt.Errorf("loading evidence of %s: %w", digest, err)
	}
	have := make(map[string]struct{}, len(present))
	for _, t := range present {
		have[t] = struct{}{}
	}

	var missing []string
	for _, g := range gates {
		rule, err := ParseGateRule(g.Rule)
		if err != nil {
			// Rules are validated when written; a bad stored rule fails
			// closed rather than letting a promotion through.
			return fmt.Errorf("stored gate %s on %s: %w", g.ID, stage.Name, err)
		}
		if _, ok := have[string(rule.Evidence)]; !ok {
			missing = append(missing, string(rule.Evidence))
		}
	}
	if len(missing) > 0 {
		return &GateError{Stage: stage.Name, Missing: missing}
	}
	return nil
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
