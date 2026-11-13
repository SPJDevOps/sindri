package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
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
// call a no-op rather than a promotable subject. scan false records the
// subject without queueing a scan.
func (r *Registry) recordSubject(ctx context.Context, q *db.Queries, repo db.Repository, digest model.Digest, size int64, kind model.SubjectKind, prov model.Provenance, scan bool) error {
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
	if prov.SourceType != "" {
		if err := q.RecordProvenance(ctx, db.RecordProvenanceParams{
			Digest:     digest.String(),
			SourceType: prov.SourceType,
			SourceRef:  optionalText(prov.SourceRef),
			BuildRef:   optionalText(prov.BuildRef),
		}); err != nil {
			return fmt.Errorf("recording provenance of %s: %w", digest, err)
		}
	}
	if !scan {
		// A referrer is a signature or a document, not an image (ADR 32
		// decision 7). It stays a subject, so the on-demand scan works.
		return nil
	}
	return r.enqueueScan(ctx, q, repo.Name, digest, kind)
}

// enqueueScan hands a hosted subject to the scanner in the publish
// transaction, so the job exists exactly when the publish commits and a
// rolled-back publish leaves none. A re-publish of an existing digest enqueues
// again (the job names the publishing repository, which bounds what the
// scanner may read): the dedupe key collapses it while a scan is queued or running, and a
// finished (or failed) scan is deliberately re-run, so a re-push after a
// failed scan rescans.
func (r *Registry) enqueueScan(ctx context.Context, q *db.Queries, repoName string, digest model.Digest, kind model.SubjectKind) error {
	if !r.scanOnPublish || !scannable(kind) {
		return nil
	}
	args, err := json.Marshal(scanjob.Args{Digest: digest.String(), Kind: string(kind), Repository: repoName})
	if err != nil {
		return fmt.Errorf("encoding scan job for %s: %w", digest, err)
	}
	if _, err := queue.EnqueueTx(ctx, q, queue.EnqueueParams{
		Kind:      scanjob.JobKind,
		Args:      args,
		DedupeKey: scanjob.DedupeKey(digest.String()),
		CreatedBy: scanjob.Actor,
	}); err != nil {
		return fmt.Errorf("enqueueing scan of %s: %w", digest, err)
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

// scannable reports whether a subject kind has contents the scanner reads.
// Files published through the API are opaque and are not scanned.
func scannable(kind model.SubjectKind) bool {
	switch kind {
	case model.SubjectKindOCIManifest, model.SubjectKindOCIIndex,
		model.SubjectKindPyPIDist, model.SubjectKindNPMTarball:
		return true
	case model.SubjectKindFile:
		return false
	}
	return false
}
