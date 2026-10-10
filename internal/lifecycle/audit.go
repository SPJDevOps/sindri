package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// MaxAuditPage bounds one page of the audit trail.
const MaxAuditPage = 500

// AuditParams filters the audit trail. Zero values filter nothing, except
// RepositoryIDs: nil means every repository, which only an administrator may
// ask for, and an empty slice means none.
type AuditParams struct {
	RepositoryIDs []string
	// Before pages backwards: only events with a smaller id.
	Before int64
	Digest model.Digest
	Stage  string
	Actor  string
	Action Action
	Since  time.Time
	Until  time.Time
	Limit  int
}

// Audit returns ledger events newest first. The ledger is append-only, so id
// order is insertion order and a cursor on it never skips or repeats an event,
// however many are appended while a caller pages.
func (s *Service) Audit(ctx context.Context, p AuditParams) ([]Event, error) {
	limit := p.Limit
	if limit <= 0 || limit > MaxAuditPage {
		limit = MaxAuditPage
	}
	var ids []uuid.UUID
	if p.RepositoryIDs != nil {
		if len(p.RepositoryIDs) == 0 {
			return []Event{}, nil
		}
		ids = make([]uuid.UUID, 0, len(p.RepositoryIDs))
		for _, raw := range p.RepositoryIDs {
			id, err := uuid.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: repository id %q", ErrInvalidInput, raw)
			}
			ids = append(ids, id)
		}
	}
	rows, err := s.q.ListAuditEvents(ctx, db.ListAuditEventsParams{
		RepoIds:       ids,
		BeforeID:      pgtype.Int8{Int64: p.Before, Valid: p.Before > 0},
		SubjectDigest: optionalText(digestText(p.Digest)),
		StageName:     optionalText(p.Stage),
		Actor:         optionalText(p.Actor),
		Action:        optionalText(string(p.Action)),
		Since:         optionalTime(p.Since),
		Until:         optionalTime(p.Until),
		PageLimit:     int32(limit), //nolint:gosec // G115: bounded by MaxAuditPage
	})
	if err != nil {
		return nil, fmt.Errorf("reading the audit trail: %w", err)
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		digest, err := model.ParseDigest(row.SubjectDigest)
		if err != nil {
			return nil, fmt.Errorf("reading the audit trail: stored digest %q: %w", row.SubjectDigest, err)
		}
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

func digestText(d model.Digest) string {
	if !d.Valid() {
		return ""
	}
	return d.String()
}

func optionalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func optionalTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
