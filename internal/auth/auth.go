// Package auth decides who a caller is and what they may do.
//
// Authentication accepts three credentials, told apart by shape: a machine
// token for CI (prefix "sindri_"), an identity-provider token (a JWT), and the
// bootstrap token from the configuration. Every one arrives the same way, as a
// Bearer token or as the password of HTTP Basic, which is what docker, pip,
// and twine send (ADR 18).
//
// Authorization is one function, Authorize, called on every route: both
// protocol adapters, the management API, and the UI. There is no anonymous
// path in v1. Group membership is read from the caller's token on every
// request and never stored, so a membership the identity provider revokes
// stops granting access without anything being re-applied (ADR 5).
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
)

var (
	// ErrUnauthorized means the request carried no credential this server
	// accepts. Adapters answer 401 with their protocol's challenge.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrForbidden means the caller is known and lacks the role. Adapters
	// answer 403.
	ErrForbidden = errors.New("forbidden")
)

// Kind says how a principal authenticated.
type Kind string

const (
	// KindUser is a person, authenticated by an identity-provider token.
	KindUser Kind = "user"
	// KindMachine is a CI job holding a machine token.
	KindMachine Kind = "machine"
	// KindBootstrap is the holder of the configured bootstrap token.
	KindBootstrap Kind = "bootstrap"
)

// Role is what a grant allows. Each role includes the ones before it.
type Role string

const (
	// RoleRead pulls and lists.
	RoleRead Role = "read"
	// RoleWrite also pushes, uploads, attaches evidence, and promotes.
	RoleWrite Role = "write"
	// RoleAdmin also creates and deletes repositories and applies
	// configuration.
	RoleAdmin Role = "admin"
)

func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleWrite:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Includes reports whether r grants at least other.
func (r Role) Includes(other Role) bool { return r.rank() >= other.rank() && other.rank() > 0 }

// Valid reports whether r is a role.
func (r Role) Valid() bool { return r.rank() > 0 }

// TokenScope is where a machine token applies. At most one of Repository and
// Namespace is set; neither means every repository.
type TokenScope struct {
	TokenID    string
	Repository string
	Namespace  string
	Role       Role
}

// Principal is an authenticated caller. It lives for one request.
type Principal struct {
	// Subject is the stable name recorded as the actor in the ledger:
	// "user:<name>", "token:<name>", or "bootstrap".
	Subject string
	Kind    Kind
	// Groups are the identity provider's groups for a user, as of the token
	// presented with this request.
	Groups []string
	// Admin administers the whole server.
	Admin bool
	// Token is set for a machine principal.
	Token *TokenScope
}

// InGroup reports whether the principal belongs to any of groups.
func (p Principal) InGroup(groups []string) bool {
	for _, g := range groups {
		if slices.Contains(p.Groups, g) {
			return true
		}
	}
	return false
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated principal, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator decides who sent a request.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// TokenVerifier verifies one kind of presented token.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Principal, error)
}

// MachineTokenPrefix starts every machine token, so the authenticator can
// route a credential without trying each verifier in turn, and so a leaked
// token is recognisable to secret scanners.
const MachineTokenPrefix = "sindri_"

// Chain is the server's Authenticator.
type Chain struct {
	bootstrap string
	machines  TokenVerifier // nil when no database is wired, in unit tests
	oidc      atomic.Pointer[verifierBox]
	admins    atomic.Pointer[[]string]
}

type verifierBox struct{ v TokenVerifier }

// NewChain builds the authenticator. An empty bootstrap token disables it.
func NewChain(bootstrap string, machines TokenVerifier, oidc TokenVerifier, admins []string) *Chain {
	c := &Chain{bootstrap: bootstrap, machines: machines}
	c.SetOIDC(oidc)
	c.SetAdmins(admins)
	return c
}

// SetAdmins replaces the administrator groups; a configuration reload calls it.
func (c *Chain) SetAdmins(groups []string) {
	cp := append([]string(nil), groups...)
	c.admins.Store(&cp)
}

// SetOIDC replaces the identity-provider verifier. Nil disables it.
func (c *Chain) SetOIDC(v TokenVerifier) {
	c.oidc.Store(&verifierBox{v: v})
}

// Authenticate implements Authenticator.
func (c *Chain) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	raw, ok := Credential(r)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	return c.Verify(ctx, raw)
}

// Verify authenticates one presented credential.
func (c *Chain) Verify(ctx context.Context, raw string) (Principal, error) {
	switch {
	case strings.HasPrefix(raw, MachineTokenPrefix):
		if c.machines == nil {
			return Principal{}, ErrUnauthorized
		}
		return c.machines.Verify(ctx, raw)

	case looksLikeJWT(raw):
		box := c.oidc.Load()
		if box == nil || box.v == nil {
			return Principal{}, ErrUnauthorized
		}
		p, err := box.v.Verify(ctx, raw)
		if err != nil {
			return Principal{}, err
		}
		p.Admin = p.InGroup(*c.admins.Load())
		return p, nil

	default:
		if c.bootstrap == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(c.bootstrap)) != 1 {
			return Principal{}, ErrUnauthorized
		}
		return Principal{Subject: "bootstrap", Kind: KindBootstrap, Admin: true}, nil
	}
}

// Credential extracts the presented secret: a Bearer token, or the password
// of HTTP Basic. The Basic username is ignored, because docker and pip require
// one and it carries nothing a token does not.
func Credential(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if scheme, cred, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
		cred = strings.TrimSpace(cred)
		return cred, cred != ""
	}
	if _, password, ok := r.BasicAuth(); ok && password != "" {
		return password, true
	}
	return "", false
}

func looksLikeJWT(raw string) bool {
	return strings.Count(raw, ".") == 2 && !strings.ContainsAny(raw, " \t")
}

// Middleware authenticates every request, stores the principal in the
// context, and lets deny write the refusal in the mounting surface's own
// error shape (OCI, PyPI, and the management API each have one).
func Middleware(a Authenticator, deny func(w http.ResponseWriter, r *http.Request, err error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r.Context(), r)
			if err != nil {
				deny(w, r, fmt.Errorf("%w: %w", ErrUnauthorized, err))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// StaticBearer accepts a single shared token as the bootstrap administrator.
// It remains for tests of surfaces that only need "an administrator"; serve
// wires Chain.
type StaticBearer struct{ chain *Chain }

// NewStaticBearer returns an Authenticator accepting token. An empty token
// rejects every request (fail closed).
func NewStaticBearer(token string) *StaticBearer {
	return &StaticBearer{chain: NewChain(token, nil, nil, nil)}
}

// Authenticate implements Authenticator.
func (s *StaticBearer) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	return s.chain.Authenticate(ctx, r)
}
