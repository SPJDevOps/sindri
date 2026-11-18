package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// StageApproval is the highest lifecycle stage a subject is currently approved
// at in one repository. A revoked stage is not approved, so a revoke of the top
// stage leaves the next one down as the highest.
type StageApproval struct {
	Repository string `json:"repository"`
	Stage      string `json:"stage"`
	Position   int32  `json:"position"`
}

// SubjectSummary is what a row or a header shows about one digest.
type SubjectSummary struct {
	Digest model.Digest
	// TotalSize is the digest's blob plus the blobs it names, two levels deep.
	TotalSize int64
	// IsSubject is false for content with no identity: proxy content, a layer.
	// Only then are CreatedAt, Owners, Approvals, and Scans empty.
	IsSubject bool
	// CreatedAt is when the digest was first published into any hosted repository.
	CreatedAt time.Time
	// Owners are the owner references as stored, across every repository. The
	// caller shows only those naming a repository the reader may read.
	Owners []string
	// Approvals lists, for each repository asked about, the highest stage the
	// subject is currently approved at; a repository with none is absent.
	Approvals []StageApproval
	// Scans is the newest Sindri-produced scan per platform, trimmed to
	// producer, platform, and severity_counts. A scan a client attached never
	// appears (ADR 29, ADR 30).
	Scans []json.RawMessage
}

// Summaries returns the summary of each digest in one query, however many
// there are. A digest the server has no blob for is absent from the result.
//
// It checks no placement (invariant I2): pass only digests already found in a
// repository the caller may read. repos limits the lifecycle state returned to
// those repositories, so a subject placed in two teams' repositories shows each
// reader only the state of the repositories they may read.
func (s *Service) Summaries(ctx context.Context, digests []model.Digest, repos []string) (map[model.Digest]SubjectSummary, error) {
	out := make(map[model.Digest]SubjectSummary, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	if repos == nil {
		repos = []string{}
	}
	args := make([]string, len(digests))
	for i, d := range digests {
		args[i] = d.String()
	}
	rows, err := s.q.SummarizeSubjects(ctx, db.SummarizeSubjectsParams{Digests: args, RepoNames: repos})
	if err != nil {
		return nil, fmt.Errorf("summarising %d digests: %w", len(digests), err)
	}
	for _, row := range rows {
		digest, err := model.ParseDigest(row.Digest)
		if err != nil {
			return nil, fmt.Errorf("stored digest %q: %w", row.Digest, err)
		}
		sum := SubjectSummary{
			Digest: digest, TotalSize: row.TotalSize, IsSubject: row.CreatedAt.Valid,
			CreatedAt: row.CreatedAt.Time, Owners: row.Owners,
		}
		if err := json.Unmarshal(row.Stages, &sum.Approvals); err != nil {
			return nil, fmt.Errorf("reading the stages of %s: %w", digest, err)
		}
		if err := json.Unmarshal(row.Scans, &sum.Scans); err != nil {
			return nil, fmt.Errorf("reading the scans of %s: %w", digest, err)
		}
		out[digest] = sum
	}
	return out, nil
}
