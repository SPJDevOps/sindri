package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// GateRule is one precondition for promotion into a stage. v1 has a single
// rule kind: the subject must carry evidence of a type. Stage order is not a
// rule (ADR 16); "must pass integration first" is not expressible in v1.
type GateRule struct {
	Evidence model.EvidenceType `json:"evidence"`
}

// ParseGateRule decodes and validates a stored or submitted rule. Unknown keys
// are rejected so that a rule this build cannot evaluate is never accepted
// and then silently ignored.
func ParseGateRule(raw []byte) (GateRule, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rule GateRule
	if err := dec.Decode(&rule); err != nil {
		return GateRule{}, fmt.Errorf("%w: gate rule: %w", ErrInvalidInput, err)
	}
	if dec.More() {
		return GateRule{}, fmt.Errorf("%w: gate rule has trailing data", ErrInvalidInput)
	}
	if err := rule.validate(); err != nil {
		return GateRule{}, err
	}
	return rule, nil
}

func (r GateRule) validate() error {
	if r.Evidence == "" {
		return fmt.Errorf("%w: gate rule must name an evidence type", ErrInvalidInput)
	}
	if !r.Evidence.Valid() {
		return fmt.Errorf("%w: unknown evidence type %q", ErrInvalidInput, r.Evidence)
	}
	return nil
}

// StageSpec declares one stage and its gates.
type StageSpec struct {
	Name  string
	Gates []GateRule
}

// Stage is a defined stage, in position order.
type Stage struct {
	Name     string
	Position int32
	Gates    []GateRule
}

// ValidateStages checks a stage set without touching the database, so the M7
// reconciler can reject a bad lifecycle section before anything is applied.
func ValidateStages(specs []StageSpec) error {
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if err := validateStageName(spec.Name); err != nil {
			return err
		}
		if _, dup := seen[spec.Name]; dup {
			return fmt.Errorf("%w: stage %q is declared twice", ErrInvalidInput, spec.Name)
		}
		seen[spec.Name] = struct{}{}

		gates := make(map[model.EvidenceType]struct{}, len(spec.Gates))
		for _, g := range spec.Gates {
			if err := g.validate(); err != nil {
				return fmt.Errorf("stage %q: %w", spec.Name, err)
			}
			if _, dup := gates[g.Evidence]; dup {
				return fmt.Errorf("%w: stage %q requires %q twice", ErrInvalidInput, spec.Name, g.Evidence)
			}
			gates[g.Evidence] = struct{}{}
		}
	}
	return nil
}

// validateStageName keeps stage names usable as an environment query value
// and as a future config key: lowercase letters, digits, '-', '_', '.'.
func validateStageName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: stage name is required", ErrInvalidInput)
	}
	if len(name) > 63 {
		return fmt.Errorf("%w: stage name %q is longer than 63 characters", ErrInvalidInput, name)
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_' || c == '.') && i > 0:
		default:
			return fmt.Errorf("%w: stage name %q must be lowercase letters, digits, '-', '_', or '.'", ErrInvalidInput, name)
		}
	}
	return nil
}

// SetStages replaces the stage set in one transaction. Positions follow the
// slice order. A stage the ledger refers to cannot be removed: its history
// would otherwise point at nothing (ErrStageInUse).
func (s *Service) SetStages(ctx context.Context, specs []StageSpec) ([]Stage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning stage update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := SetStagesIn(ctx, s.q.WithTx(tx), specs); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing stage update: %w", err)
	}
	return s.ListStages(ctx)
}

// SetStagesIn replaces the stage set using q, which the caller binds to a
// transaction. The reconciler uses it to commit stages together with the
// repositories that refer to them; the management API uses SetStages. One
// validator and one writer serve both (ADR 16).
func SetStagesIn(ctx context.Context, q *db.Queries, specs []StageSpec) error {
	if err := ValidateStages(specs); err != nil {
		return err
	}
	keep := make([]string, 0, len(specs))
	for _, spec := range specs {
		keep = append(keep, spec.Name)
	}

	inUse, err := q.ListStageNamesWithHistoryNotIn(ctx, keep)
	if err != nil {
		return fmt.Errorf("checking stage history: %w", err)
	}
	if len(inUse) > 0 {
		return fmt.Errorf("%w: cannot remove %s", ErrStageInUse, strings.Join(inUse, ", "))
	}
	if _, err := q.DeleteStagesNotIn(ctx, keep); err != nil {
		return fmt.Errorf("removing stages: %w", err)
	}

	for i, spec := range specs {
		row, err := q.UpsertStage(ctx, db.UpsertStageParams{Name: spec.Name, Position: int32(i)}) //nolint:gosec // G115: bounded by request size, far below int32
		if err != nil {
			return fmt.Errorf("writing stage %s: %w", spec.Name, err)
		}
		if err := q.DeleteGatesForStage(ctx, row.ID); err != nil {
			return fmt.Errorf("clearing gates of %s: %w", spec.Name, err)
		}
		for j, g := range spec.Gates {
			rule, err := marshalRule(g)
			if err != nil {
				return err
			}
			if err := q.InsertGate(ctx, db.InsertGateParams{
				StageID:  row.ID,
				Position: int32(j), //nolint:gosec // G115: bounded by request size, far below int32
				Rule:     rule,
			}); err != nil {
				return fmt.Errorf("writing gate on %s: %w", spec.Name, err)
			}
		}
	}

	return nil
}

// ListStages returns the stages in position order with their gates.
func (s *Service) ListStages(ctx context.Context) ([]Stage, error) {
	return ListStagesIn(ctx, s.q)
}

// ListStagesIn is ListStages through q, so a caller holding a transaction
// reads its own uncommitted writes.
func ListStagesIn(ctx context.Context, q *db.Queries) ([]Stage, error) {
	rows, err := q.ListStages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing stages: %w", err)
	}
	gates, err := q.ListGates(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing gates: %w", err)
	}
	byStage := make(map[string][]GateRule, len(rows))
	for _, g := range gates {
		rule, err := ParseGateRule(g.Rule)
		if err != nil {
			return nil, fmt.Errorf("stored gate %s on %s: %w", g.ID, g.StageName, err)
		}
		byStage[g.StageName] = append(byStage[g.StageName], rule)
	}

	stages := make([]Stage, 0, len(rows))
	for _, row := range rows {
		gates := byStage[row.Name]
		if gates == nil {
			gates = []GateRule{}
		}
		stages = append(stages, Stage{Name: row.Name, Position: row.Position, Gates: gates})
	}
	return stages, nil
}
