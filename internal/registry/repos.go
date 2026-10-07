package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

var (
	// ErrReadOnly means the repository does not accept writes (proxy or virtual).
	ErrReadOnly = errors.New("repository is read-only")

	// ErrCycle means a virtual member list would introduce a membership cycle.
	ErrCycle = errors.New("virtual membership cycle")
)

// Remote describes a proxy repository's upstream.
type Remote struct {
	RepoID         string
	UpstreamURL    string
	CredentialsRef string
	TTL            time.Duration
	NegativeTTL    time.Duration
}

// CreateRepositoryParams is the input to CreateRepository.
type CreateRepositoryParams struct {
	Name     string
	Type     model.RepoType
	Protocol model.Protocol

	// UpstreamURL is required for proxy repositories.
	UpstreamURL    string
	CredentialsRef string
	TTLSeconds     int32
	NegativeTTL    int32 // seconds; named to match the remotes column

	// Members is required for virtual repositories: ordered repository names.
	Members []string

	// LifecycleStages, when non-empty, is rejected for proxy and virtual
	// repositories (invariant I4). The management API passes it through
	// reconcile.ValidateRepository before create. Non-empty restricts
	// promotion to the listed stages; empty permits every stage.
	LifecycleStages []string

	// Access lists the groups that may read and write.
	Access model.Access
}

// CreateRepository inserts a repository managed by the API. Proxy rows also
// get a remotes row; virtual rows get repository_members.
func (r *Registry) CreateRepository(ctx context.Context, p CreateRepositoryParams) (model.Repository, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return model.Repository{}, fmt.Errorf("%w: repository name is required", ErrInvalidInput)
	}
	if err := ValidateRepoName(name); err != nil {
		return model.Repository{}, err
	}
	if p.Type == "" {
		p.Type = model.RepoTypeHosted
	}
	if p.Type != model.RepoTypeHosted && p.Type != model.RepoTypeProxy && p.Type != model.RepoTypeVirtual {
		return model.Repository{}, fmt.Errorf("%w: repository type %q", ErrInvalidInput, p.Type)
	}
	if p.Protocol != model.ProtocolOCI && p.Protocol != model.ProtocolPyPI {
		return model.Repository{}, fmt.Errorf("%w: protocol %q (want oci or pypi)", ErrInvalidInput, p.Protocol)
	}
	if err := validateCreateParams(p); err != nil {
		return model.Repository{}, err
	}

	var lc *model.RepoLifecycle
	if len(p.LifecycleStages) > 0 {
		lc = &model.RepoLifecycle{Stages: p.LifecycleStages}
	}
	cfg, err := marshalRepoConfig("", "", p.Access, lc)
	if err != nil {
		return model.Repository{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.Repository{}, fmt.Errorf("beginning create repository transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	row, err := q.CreateRepository(ctx, db.CreateRepositoryParams{
		Name:      name,
		Type:      string(p.Type),
		Protocol:  string(p.Protocol),
		Config:    cfg,
		Locked:    false,
		ManagedBy: string(model.ManagedByAPI),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return model.Repository{}, fmt.Errorf("creating repository %s: %w", name, ErrAlreadyExists)
		}
		return model.Repository{}, fmt.Errorf("creating repository %s: %w", name, err)
	}

	switch p.Type {
	case model.RepoTypeProxy:
		cred := pgtype.Text{}
		if p.CredentialsRef != "" {
			cred = pgtype.Text{String: p.CredentialsRef, Valid: true}
		}
		if _, err := q.UpsertRemote(ctx, db.UpsertRemoteParams{
			RepoID:             row.ID,
			UpstreamUrl:        strings.TrimRight(p.UpstreamURL, "/"),
			CredentialsRef:     cred,
			TtlSeconds:         p.TTLSeconds,
			NegativeTtlSeconds: p.NegativeTTL,
		}); err != nil {
			return model.Repository{}, fmt.Errorf("recording remote for %s: %w", name, err)
		}
	case model.RepoTypeVirtual:
		memberIDs, err := r.resolveMemberIDs(ctx, q, row.ID, p.Protocol, p.Members)
		if err != nil {
			return model.Repository{}, err
		}
		for i, memberID := range memberIDs {
			if _, err := q.InsertRepositoryMember(ctx, db.InsertRepositoryMemberParams{
				VirtualRepoID: row.ID,
				MemberRepoID:  memberID,
				Position:      int32(i),
			}); err != nil {
				return model.Repository{}, fmt.Errorf("recording member %s of %s: %w", p.Members[i], name, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Repository{}, fmt.Errorf("committing create repository: %w", err)
	}
	return repositoryFromRow(row), nil
}

func validateCreateParams(p CreateRepositoryParams) error {
	switch p.Type {
	case model.RepoTypeHosted:
		if p.UpstreamURL != "" || len(p.Members) > 0 {
			return fmt.Errorf("%w: hosted repositories do not take upstream or members", ErrInvalidInput)
		}
	case model.RepoTypeProxy:
		if len(p.Members) > 0 {
			return fmt.Errorf("%w: proxy repositories do not take members", ErrInvalidInput)
		}
		if strings.TrimSpace(p.UpstreamURL) == "" {
			return fmt.Errorf("%w: proxy repositories require upstream_url", ErrInvalidInput)
		}
		u, err := url.Parse(p.UpstreamURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: upstream_url must be an http(s) URL", ErrInvalidInput)
		}
		if p.TTLSeconds < 0 || p.NegativeTTL < 0 {
			return fmt.Errorf("%w: ttl values must be non-negative", ErrInvalidInput)
		}
	case model.RepoTypeVirtual:
		if p.UpstreamURL != "" {
			return fmt.Errorf("%w: virtual repositories do not take upstream_url", ErrInvalidInput)
		}
		if len(p.Members) == 0 {
			return fmt.Errorf("%w: virtual repositories require members", ErrInvalidInput)
		}
	}
	return nil
}

func (r *Registry) resolveMemberIDs(ctx context.Context, q *db.Queries, virtualID uuid.UUID, protocol model.Protocol, members []string) ([]uuid.UUID, error) {
	seen := make(map[string]struct{}, len(members))
	ids := make([]uuid.UUID, 0, len(members))
	for _, name := range members {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%w: empty member name", ErrInvalidInput)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: duplicate member %q", ErrInvalidInput, name)
		}
		seen[name] = struct{}{}

		member, err := q.GetRepositoryByName(ctx, name)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: member repository %q does not exist", ErrInvalidInput, name)
			}
			return nil, fmt.Errorf("looking up member %s: %w", name, err)
		}
		if member.ID == virtualID {
			return nil, fmt.Errorf("%w: virtual repository cannot contain itself", ErrInvalidInput)
		}
		if member.Protocol != string(protocol) {
			return nil, fmt.Errorf("%w: member %q speaks %s, want %s",
				ErrInvalidInput, name, member.Protocol, protocol)
		}
		if member.Type == string(model.RepoTypeVirtual) {
			if err := r.detectMemberCycle(ctx, q, virtualID, member.ID, map[uuid.UUID]struct{}{virtualID: {}}); err != nil {
				return nil, err
			}
		}
		ids = append(ids, member.ID)
	}
	return ids, nil
}

func (r *Registry) detectMemberCycle(ctx context.Context, q *db.Queries, root, current uuid.UUID, visiting map[uuid.UUID]struct{}) error {
	if _, seen := visiting[current]; seen {
		return fmt.Errorf("%w: membership cycle involving %s", ErrCycle, current)
	}
	visiting[current] = struct{}{}
	children, err := q.ListRepositoryMembers(ctx, current)
	if err != nil {
		return fmt.Errorf("walking members of %s: %w", current, err)
	}
	for _, child := range children {
		if child.ID == root {
			return fmt.Errorf("%w: membership cycle", ErrCycle)
		}
		if child.Type == string(model.RepoTypeVirtual) {
			next := copyUUIDSet(visiting)
			if err := r.detectMemberCycle(ctx, q, root, child.ID, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyUUIDSet(in map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	out := make(map[uuid.UUID]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

// GetRemote returns the upstream configuration for a proxy repository.
func (r *Registry) GetRemote(ctx context.Context, repoName string) (Remote, error) {
	row, err := r.q.GetRemoteByRepoName(ctx, repoName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Remote{}, ErrNotFound
		}
		return Remote{}, fmt.Errorf("getting remote for %s: %w", repoName, err)
	}
	cred := ""
	if row.CredentialsRef.Valid {
		cred = row.CredentialsRef.String
	}
	return Remote{
		RepoID:         row.RepoID.String(),
		UpstreamURL:    row.UpstreamUrl,
		CredentialsRef: cred,
		TTL:            time.Duration(row.TtlSeconds) * time.Second,
		NegativeTTL:    time.Duration(row.NegativeTtlSeconds) * time.Second,
	}, nil
}

// ListMembers returns the ordered member repositories of a virtual repository.
func (r *Registry) ListMembers(ctx context.Context, virtualName string) ([]model.Repository, error) {
	rows, err := r.q.ListRepositoryMembersByName(ctx, virtualName)
	if err != nil {
		return nil, fmt.Errorf("listing members of %s: %w", virtualName, err)
	}
	out := make([]model.Repository, 0, len(rows))
	for _, row := range rows {
		out = append(out, repositoryFromColumns(row.ID, row.Name, row.Type, row.Protocol, row.Config,
			row.Locked, row.ManagedBy, row.CreatedAt))
	}
	return out, nil
}

// PackageClaimed reports whether a hosted repository has ever published the
// normalized package name. Used by the name-claim rule (I5).
func (r *Registry) PackageClaimed(ctx context.Context, repoName, normalizedName string) (bool, error) {
	repo, err := r.q.GetRepositoryByName(ctx, repoName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("looking up repository %s: %w", repoName, err)
	}
	exists, err := r.q.PackageExistsInRepo(ctx, db.PackageExistsInRepoParams{
		RepoID:         repo.ID,
		NormalizedName: normalizedName,
	})
	if err != nil {
		return false, fmt.Errorf("checking package claim %s/%s: %w", repoName, normalizedName, err)
	}
	return exists, nil
}

// MatchRepositoryPrefix finds the longest repository name that is a prefix of
// path (equal, or followed by '/'). Used by OCI routing when an image name
// sits inside a proxy or virtual repository.
func (r *Registry) MatchRepositoryPrefix(ctx context.Context, path string) (model.Repository, string, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return model.Repository{}, "", ErrNotFound
	}
	rows, err := r.q.ListRepositoryNamePrefixes(ctx, path)
	if err != nil {
		return model.Repository{}, "", fmt.Errorf("matching repository prefix for %s: %w", path, err)
	}
	if len(rows) == 0 {
		return model.Repository{}, "", ErrNotFound
	}
	best := rows[0]
	remainder := strings.TrimPrefix(path, best.Name)
	remainder = strings.TrimPrefix(remainder, "/")
	return repositoryFromRow(best), remainder, nil
}

// RequireWritableHosted returns the repository when it is a hosted repository
// of the expected protocol. Proxy and virtual repositories reject writes.
func (r *Registry) RequireWritableHosted(ctx context.Context, name string, protocol model.Protocol) (model.Repository, error) {
	repo, err := r.GetRepository(ctx, name)
	if err != nil {
		return model.Repository{}, err
	}
	if repo.Protocol != protocol {
		return model.Repository{}, ErrWrongProtocol
	}
	if repo.Type != model.RepoTypeHosted {
		return model.Repository{}, fmt.Errorf("%w: %s is %s", ErrReadOnly, name, repo.Type)
	}
	return repo, nil
}

// CacheStatus is the stored status of a remote_cache row.
type CacheStatus string

const (
	// CacheHit records a successful upstream fetch.
	CacheHit CacheStatus = "hit"
	// CacheMiss records an upstream 404, and nothing else.
	CacheMiss CacheStatus = "miss"
)

// CacheEntry is one remote_cache row.
type CacheEntry struct {
	RepoID     string
	Key        string
	Status     CacheStatus
	BlobDigest string // empty for miss rows
	FetchedAt  time.Time
}

// GetCacheEntry loads a remote_cache row.
func (r *Registry) GetCacheEntry(ctx context.Context, repoName, key string) (CacheEntry, error) {
	repo, err := r.q.GetRepositoryByName(ctx, repoName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CacheEntry{}, ErrNotFound
		}
		return CacheEntry{}, fmt.Errorf("looking up repository %s: %w", repoName, err)
	}
	row, err := r.q.GetRemoteCache(ctx, db.GetRemoteCacheParams{
		RepoID:   repo.ID,
		CacheKey: key,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CacheEntry{}, ErrNotFound
		}
		return CacheEntry{}, fmt.Errorf("getting cache %s/%s: %w", repoName, key, err)
	}
	digest := ""
	if row.BlobDigest.Valid {
		digest = row.BlobDigest.String
	}
	return CacheEntry{
		RepoID:     row.RepoID.String(),
		Key:        row.CacheKey,
		Status:     CacheStatus(row.Status),
		BlobDigest: digest,
		FetchedAt:  row.FetchedAt,
	}, nil
}

// PutCacheEntry writes a remote_cache row. digest may be empty for miss rows.
func (r *Registry) PutCacheEntry(ctx context.Context, repoName, key string, status CacheStatus, digest string) error {
	repo, err := r.q.GetRepositoryByName(ctx, repoName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("looking up repository %s: %w", repoName, err)
	}
	blob := pgtype.Text{}
	if digest != "" {
		blob = pgtype.Text{String: digest, Valid: true}
	}
	_, err = r.q.UpsertRemoteCache(ctx, db.UpsertRemoteCacheParams{
		RepoID:     repo.ID,
		CacheKey:   key,
		Status:     string(status),
		BlobDigest: blob,
		FetchedAt:  time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("writing cache %s/%s: %w", repoName, key, err)
	}
	return nil
}

// RepoUUID returns the UUID for a repository name.
func (r *Registry) RepoUUID(ctx context.Context, name string) (uuid.UUID, error) {
	row, err := r.q.GetRepositoryByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.UUID{}, ErrNotFound
		}
		return uuid.UUID{}, fmt.Errorf("looking up repository %s: %w", name, err)
	}
	return row.ID, nil
}
