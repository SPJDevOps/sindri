package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// BlockedDigest is the audit record of one digest an operator evicted from an
// OCI proxy repository and refuses to fetch again (SIN-73, ADR 14).
type BlockedDigest struct {
	Digest    model.Digest
	BlockedBy string
	Reason    string
	BlockedAt time.Time
}

// requireOCIProxy loads an OCI proxy repository. Anything else is invalid
// input: blocking is about what a proxy may fetch, and a hosted repository
// holds only what was pushed to it.
func (r *Registry) requireOCIProxy(ctx context.Context, name string) (db.Repository, error) {
	repo, err := r.requireOCIRepo(ctx, name)
	if err != nil {
		if errors.Is(err, ErrWrongProtocol) {
			return db.Repository{}, fmt.Errorf("%w: %s is not an OCI repository", ErrInvalidInput, name)
		}
		return db.Repository{}, err
	}
	if repo.Type != string(model.RepoTypeProxy) {
		return db.Repository{}, fmt.Errorf("%w: %s is %s, not a proxy repository", ErrInvalidInput, name, repo.Type)
	}
	return repo, nil
}

// EvictDigest removes one digest, and what only it justified, from an OCI
// proxy repository and records a block so it is never fetched there again.
// It unlinks the placement, deletes tags that point at it and the cache
// entries that justified them, and for a manifest or index also unlinks each
// descendant that no other manifest still placed in this repository lists.
// Only the requested digest is blocked. Bytes are never touched (ADR 19): the
// collector deletes them once nothing places them, and manifest_subjects rows,
// which every repository shares, stay. A digest that is not placed is still
// blocked, so the call is idempotent and works pre-emptively.
func (r *Registry) EvictDigest(ctx context.Context, repoName string, digest model.Digest, actor, reason string) (BlockedDigest, error) {
	if !digest.Valid() {
		return BlockedDigest{}, fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	repo, err := r.requireOCIProxy(ctx, repoName)
	if err != nil {
		return BlockedDigest{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BlockedDigest{}, fmt.Errorf("beginning eviction of %s: %w", digest, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := r.q.WithTx(tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, repo.ID.String()+"|"+digest.String()); err != nil {
		return BlockedDigest{}, fmt.Errorf("locking %s in %s: %w", digest, repoName, err)
	}

	// Record the block first: it serializes with a concurrent fetch that
	// places the digest, which checks it in its own transaction.
	row, err := q.InsertBlockedDigest(ctx, db.InsertBlockedDigestParams{
		RepoID: repo.ID, Digest: digest.String(), BlockedBy: actor, Reason: reason,
	})
	if err != nil {
		return BlockedDigest{}, fmt.Errorf("recording block of %s: %w", digest, err)
	}

	gone, err := evictableDescendants(ctx, q, repo.ID, digest.String())
	if err != nil {
		return BlockedDigest{}, err
	}
	for _, d := range append([]string{digest.String()}, gone...) {
		if _, err := q.UnlinkRepositoryBlob(ctx, db.UnlinkRepositoryBlobParams{RepoID: repo.ID, Digest: d}); err != nil {
			return BlockedDigest{}, fmt.Errorf("unlinking %s from %s: %w", d, repoName, err)
		}
		if err := q.DeleteTagsByDigest(ctx, db.DeleteTagsByDigestParams{RepoID: repo.ID, TargetDigest: d}); err != nil {
			return BlockedDigest{}, fmt.Errorf("deleting tags of %s: %w", d, err)
		}
		if _, err := q.DeleteRemoteCacheByBlobDigest(ctx, db.DeleteRemoteCacheByBlobDigestParams{
			RepoID: repo.ID, BlobDigest: pgtype.Text{String: d, Valid: true},
		}); err != nil {
			return BlockedDigest{}, fmt.Errorf("deleting cache entries of %s: %w", d, err)
		}
		if _, err := q.DeleteRemoteCacheByManifestDigestKey(ctx, db.DeleteRemoteCacheByManifestDigestKeyParams{
			RepoID: repo.ID, Prefix: cacheKeyOCIManifestPrefix, Digest: d,
		}); err != nil {
			return BlockedDigest{}, fmt.Errorf("deleting digest cache entries of %s: %w", d, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BlockedDigest{}, fmt.Errorf("committing eviction of %s: %w", digest, err)
	}
	return blockedFromRow(row.Digest, row.BlockedBy, row.Reason, row.BlockedAt)
}

// evictableDescendants returns the digests below root, through
// manifest_children, that no manifest placed in the repository and outside the
// eviction still lists. It computes the whole closure, then keeps shrinking it
// until nothing outside it references anything inside it: a kept child keeps
// its own children too.
func evictableDescendants(ctx context.Context, q *db.Queries, repoID uuid.UUID, root string) ([]string, error) {
	seen := map[string]bool{root: true}
	queue := []string{root}
	var candidates []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		children, err := q.ListManifestChildren(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("listing children of %s: %w", cur, err)
		}
		for _, c := range children {
			if !seen[c] {
				seen[c] = true
				candidates = append(candidates, c)
				queue = append(queue, c)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		evicting := append([]string{root}, candidates...)
		for _, c := range slices.Clone(candidates) {
			referenced, err := q.IsChildReferencedInRepo(ctx, db.IsChildReferencedInRepoParams{
				RepoID: repoID, Child: c, Evicting: evicting,
			})
			if err != nil {
				return nil, fmt.Errorf("checking references to %s: %w", c, err)
			}
			if referenced {
				candidates = slices.DeleteFunc(candidates, func(s string) bool { return s == c })
				changed = true
			}
		}
	}
	return candidates, nil
}

// UnblockDigest lifts a block. It reports whether one existed. The digest is
// not placed again: the next pull fetches and places it as for any miss.
func (r *Registry) UnblockDigest(ctx context.Context, repoName string, digest model.Digest) error {
	if !digest.Valid() {
		return fmt.Errorf("%w: digest is required", ErrInvalidInput)
	}
	repo, err := r.requireOCIProxy(ctx, repoName)
	if err != nil {
		return err
	}
	n, err := r.q.DeleteBlockedDigest(ctx, db.DeleteBlockedDigestParams{RepoID: repo.ID, Digest: digest.String()})
	if err != nil {
		return fmt.Errorf("removing block of %s: %w", digest, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListBlockedDigests lists the blocks of one OCI proxy repository.
func (r *Registry) ListBlockedDigests(ctx context.Context, repoName string) ([]BlockedDigest, error) {
	repo, err := r.requireOCIProxy(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListBlockedDigests(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("listing blocked digests of %s: %w", repoName, err)
	}
	out := make([]BlockedDigest, 0, len(rows))
	for _, row := range rows {
		b, err := blockedFromRow(row.Digest, row.BlockedBy, row.Reason, row.BlockedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// IsDigestBlocked reports whether digest is blocked in the named repository.
// A proxy checks it before any upstream call and before it records a
// placement, so a blocked digest is never fetched again.
func (r *Registry) IsDigestBlocked(ctx context.Context, repoName string, digest model.Digest) (bool, error) {
	blocked, err := r.q.IsDigestBlocked(ctx, db.IsDigestBlockedParams{RepoName: repoName, Digest: digest.String()})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("checking block of %s in %s: %w", digest, repoName, err)
	}
	return blocked, nil
}

func blockedFromRow(digest, by, reason string, at time.Time) (BlockedDigest, error) {
	d, err := model.ParseDigest(digest)
	if err != nil {
		return BlockedDigest{}, fmt.Errorf("stored blocked digest %q: %w", digest, err)
	}
	return BlockedDigest{Digest: d, BlockedBy: by, Reason: reason, BlockedAt: at}, nil
}

// ErrBlocked means an operator blocked the digest in this repository. It wraps
// ErrNotFound, so every adapter answers it as the protocol's "unknown" error
// and a virtual walk counts the member as a miss.
var ErrBlocked = fmt.Errorf("%w: digest is blocked in this repository", ErrNotFound)

// refuseBlocked fails with ErrBlocked when digest is blocked in the repository.
// It takes the same per-digest advisory lock as EvictDigest, so a placement
// racing an eviction either sees the block or is unlinked by the eviction.
func refuseBlocked(ctx context.Context, tx pgx.Tx, q *db.Queries, repoID uuid.UUID, repoName string, digest model.Digest) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, repoID.String()+"|"+digest.String()); err != nil {
		return fmt.Errorf("locking %s in %s: %w", digest, repoName, err)
	}
	blocked, err := q.IsDigestBlocked(ctx, db.IsDigestBlockedParams{RepoName: repoName, Digest: digest.String()})
	if err != nil {
		return fmt.Errorf("checking block of %s in %s: %w", digest, repoName, err)
	}
	if blocked {
		return ErrBlocked
	}
	return nil
}
