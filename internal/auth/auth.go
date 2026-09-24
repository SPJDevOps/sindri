// Package auth authenticates callers of the management API.
//
// M2 ships a static bearer token as an explicit placeholder. M7 replaces the
// Authenticator implementation (OIDC, RBAC) without changing the middleware
// hook that every /api/v1 route already goes through.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrUnauthorized means the request did not present acceptable credentials.
var ErrUnauthorized = errors.New("unauthorized")

// Identity is who the authenticator accepted. M7 fills this out; M2 only needs
// a stable subject string for logs.
type Identity struct {
	Subject string
}

type identityKey struct{}

// IdentityFromContext returns the authenticated identity, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Authenticator decides whether a request is allowed. M7 swaps implementations
// behind this interface; the middleware stays.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Identity, error)
}

// StaticBearer accepts a single shared bearer token from configuration.
type StaticBearer struct {
	token string
}

// NewStaticBearer returns an Authenticator that accepts Authorization: Bearer
// matching token. An empty token rejects every request (fail closed).
func NewStaticBearer(token string) *StaticBearer {
	return &StaticBearer{token: token}
}

// Authenticate implements Authenticator.
func (s *StaticBearer) Authenticate(_ context.Context, r *http.Request) (Identity, error) {
	if s.token == "" {
		return Identity{}, ErrUnauthorized
	}
	header := r.Header.Get("Authorization")
	scheme, cred, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Identity{}, ErrUnauthorized
	}
	cred = strings.TrimSpace(cred)
	if subtle.ConstantTimeCompare([]byte(cred), []byte(s.token)) != 1 {
		return Identity{}, ErrUnauthorized
	}
	return Identity{Subject: "static"}, nil
}

// Middleware rejects requests the Authenticator does not accept.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r.Context(), r)
			if err != nil {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("WWW-Authenticate", `Bearer realm="sindri"`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"bearer token required"}`))
				return
			}
			ctx := context.WithValue(r.Context(), identityKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
