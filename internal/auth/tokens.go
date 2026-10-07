package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
)

var (
	// ErrTokenNotFound means no machine token has that id.
	ErrTokenNotFound = errors.New("machine token not found")
	// ErrInvalidToken means a token request is malformed.
	ErrInvalidToken = errors.New("invalid machine token request")
)

// TokenService issues, verifies, and revokes machine tokens for CI.
//
// A token is MachineTokenPrefix followed by 32 random bytes. Only its SHA-256
// is stored: the token carries 256 bits of entropy, so a fast hash is a
// sufficient lookup key, and a database dump holds nothing usable (ADR 18).
type TokenService struct {
	q   *db.Queries
	now func() time.Time
}

// NewTokenService wires a TokenService onto the shared database.
func NewTokenService(database *db.DB) *TokenService {
	if database == nil {
		panic("auth.NewTokenService: nil database")
	}
	return &TokenService{q: database.Queries(), now: time.Now}
}

// IssueParams describes a new token.
type IssueParams struct {
	Name string
	// Repository scopes the token to one repository, by name.
	Repository string
	// Namespace scopes the token to a team's repositories.
	Namespace string
	Role      Role
	CreatedBy string
	ExpiresAt *time.Time
}

// Token describes an issued token. The secret is never part of it.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Repository string     `json:"repository,omitempty"`
	Namespace  string     `json:"namespace,omitempty"`
	Role       Role       `json:"role"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Issue creates a token and returns its secret, which is shown once.
func (s *TokenService) Issue(ctx context.Context, p IssueParams) (Token, string, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 128 {
		return Token{}, "", fmt.Errorf("%w: a name of 1 to 128 characters is required", ErrInvalidToken)
	}
	if !p.Role.Valid() {
		return Token{}, "", fmt.Errorf("%w: role %q, want read, write, or admin", ErrInvalidToken, p.Role)
	}
	if p.Repository != "" && p.Namespace != "" {
		return Token{}, "", fmt.Errorf("%w: scope a token to a repository or a namespace, not both", ErrInvalidToken)
	}
	if p.Role == RoleAdmin && p.Repository != "" {
		return Token{}, "", fmt.Errorf("%w: the admin role applies to a namespace or the whole server, not one repository", ErrInvalidToken)
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(s.now()) {
		return Token{}, "", fmt.Errorf("%w: expires_at is in the past", ErrInvalidToken)
	}

	var repoID pgtype.UUID
	if p.Repository != "" {
		row, err := s.q.GetRepositoryByName(ctx, p.Repository)
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, "", fmt.Errorf("%w: repository %q does not exist", ErrInvalidToken, p.Repository)
		}
		if err != nil {
			return Token{}, "", fmt.Errorf("looking up repository %s: %w", p.Repository, err)
		}
		repoID = pgtype.UUID{Bytes: row.ID, Valid: true}
	}

	secret, hash, err := newSecret()
	if err != nil {
		return Token{}, "", err
	}
	row, err := s.q.CreateMachineToken(ctx, db.CreateMachineTokenParams{
		Name:      p.Name,
		TokenHash: hash,
		RepoID:    repoID,
		Namespace: optionalText(p.Namespace),
		Role:      string(p.Role),
		CreatedBy: p.CreatedBy,
		ExpiresAt: optionalTime(p.ExpiresAt),
	})
	if err != nil {
		return Token{}, "", fmt.Errorf("recording machine token %s: %w", p.Name, err)
	}
	return Token{
		ID:         row.ID.String(),
		Name:       row.Name,
		Repository: p.Repository,
		Namespace:  p.Namespace,
		Role:       p.Role,
		CreatedBy:  row.CreatedBy,
		CreatedAt:  row.CreatedAt,
		ExpiresAt:  timePtr(row.ExpiresAt),
	}, secret, nil
}

// Verify implements TokenVerifier.
func (s *TokenService) Verify(ctx context.Context, raw string) (Principal, error) {
	sum := sha256.Sum256([]byte(raw))
	row, err := s.q.GetMachineTokenByHash(ctx, sum[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("looking up machine token: %w", err)
	}
	if row.RevokedAt.Valid {
		return Principal{}, fmt.Errorf("%w: token %q was revoked", ErrUnauthorized, row.Name)
	}
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(s.now()) {
		return Principal{}, fmt.Errorf("%w: token %q expired", ErrUnauthorized, row.Name)
	}
	scope := &TokenScope{TokenID: row.ID.String(), Role: Role(row.Role)}
	if row.RepoName.Valid {
		scope.Repository = row.RepoName.String
	}
	if row.Namespace.Valid {
		scope.Namespace = row.Namespace.String
	}
	return Principal{
		Subject: "token:" + row.Name,
		Kind:    KindMachine,
		Token:   scope,
		Admin:   scope.Role == RoleAdmin && scope.Repository == "" && scope.Namespace == "",
	}, nil
}

// List returns every token, newest last, without secrets.
func (s *TokenService) List(ctx context.Context) ([]Token, error) {
	rows, err := s.q.ListMachineTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing machine tokens: %w", err)
	}
	out := make([]Token, 0, len(rows))
	for _, row := range rows {
		t := Token{
			ID:        row.ID.String(),
			Name:      row.Name,
			Role:      Role(row.Role),
			CreatedBy: row.CreatedBy,
			CreatedAt: row.CreatedAt,
			ExpiresAt: timePtr(row.ExpiresAt),
			RevokedAt: timePtr(row.RevokedAt),
		}
		if row.RepoName.Valid {
			t.Repository = row.RepoName.String
		}
		if row.Namespace.Valid {
			t.Namespace = row.Namespace.String
		}
		out = append(out, t)
	}
	return out, nil
}

// Revoke stops a token from authenticating. The row stays, so the ledger's
// "token:<name>" actors keep meaning something.
func (s *TokenService) Revoke(ctx context.Context, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return ErrTokenNotFound
	}
	n, err := s.q.RevokeMachineToken(ctx, parsed)
	if err != nil {
		return fmt.Errorf("revoking machine token %s: %w", id, err)
	}
	if n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

func newSecret() (string, []byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generating machine token: %w", err)
	}
	secret := MachineTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(secret))
	return secret, sum[:], nil
}

func optionalText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func optionalTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
