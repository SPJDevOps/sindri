package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// OwnerPrefix starts a repository owner reference.
const OwnerPrefix = "repository:"

// OwnerRef is the owner recorded for a subject published into a repository.
// Authorization on a subject goes through its owning repositories' access
// lists (ADR 16, ADR 18).
func OwnerRef(repoName string) string {
	return OwnerPrefix + repoName
}

// recordSubject makes digest a subject owned by repo and records the
// publisher's provenance claim. It runs inside the publish transaction, so a
// placement never exists without its subject.
//
// Only hosted repositories hold artifacts that are ours (invariant I4). A
// proxy cache path must never call this, and the guard below makes a mistaken
// call a no-op rather than a promotable subject.
func recordSubject(ctx context.Context, q *db.Queries, repo db.Repository, digest model.Digest, size int64, kind model.SubjectKind, prov model.Provenance) error {
	if repo.Type != string(model.RepoTypeHosted) {
		return nil
	}
	if err := q.UpsertSubject(ctx, db.UpsertSubjectParams{
		Digest: digest.String(),
		Size:   size,
		Kind:   string(kind),
	}); err != nil {
		return fmt.Errorf("recording subject %s: %w", digest, err)
	}
	if err := q.AddSubjectOwner(ctx, db.AddSubjectOwnerParams{
		Digest:   digest.String(),
		OwnerRef: OwnerRef(repo.Name),
	}); err != nil {
		return fmt.Errorf("recording owner of %s: %w", digest, err)
	}
	if prov.SourceType == "" {
		return nil
	}
	if err := q.RecordProvenance(ctx, db.RecordProvenanceParams{
		Digest:     digest.String(),
		SourceType: prov.SourceType,
		SourceRef:  optionalText(prov.SourceRef),
		BuildRef:   optionalText(prov.BuildRef),
	}); err != nil {
		return fmt.Errorf("recording provenance of %s: %w", digest, err)
	}
	return nil
}

func optionalText(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}
