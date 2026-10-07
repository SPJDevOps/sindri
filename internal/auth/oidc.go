package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig configures token validation against an OpenID Connect provider.
type OIDCConfig struct {
	Issuer   string
	ClientID string
	// GroupsClaim names the claim listing the caller's groups.
	GroupsClaim string
	// UsernameClaim names the claim recorded as the actor; "sub" is used
	// when the token lacks it.
	UsernameClaim string
	// HTTPClient reaches the provider. Nil uses http.DefaultClient.
	HTTPClient *http.Client
}

// ErrProviderUnavailable means discovery has not succeeded yet. Requests
// carrying an identity-provider token are refused until it does; the
// bootstrap and machine tokens keep working, so an identity-provider outage
// does not lock out CI or the operator.
var ErrProviderUnavailable = errors.New("identity provider unavailable")

// OIDC verifies identity-provider ID tokens: signature against the provider's
// JWKS, issuer, audience (the client id), and expiry. go-oidc caches the JWKS
// and fetches it again when a token names a key it has not seen, which is how
// a provider's key rotation is followed without a restart.
//
// Groups come from the token presented with each request and are never
// stored (ADR 5). A membership change at the provider takes effect with the
// next token the user obtains, without anything in Sindri being re-applied.
type OIDC struct {
	cfg OIDCConfig
	// base carries the HTTP client for discovery and key fetches. It is the
	// server's lifetime context, so a provider that answers only after boot
	// can still be discovered.
	base context.Context

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	lastTry  time.Time
}

// discoveryBackoff spaces discovery attempts while the provider is down, so a
// burst of requests does not become a burst of discovery calls.
const discoveryBackoff = 5 * time.Second

// NewOIDC returns a verifier and attempts discovery once. A failed attempt is
// returned for logging but does not prevent use: discovery is retried on
// demand.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.UsernameClaim == "" {
		cfg.UsernameClaim = "email"
	}
	o := &OIDC{cfg: cfg, base: ctx}
	if cfg.HTTPClient != nil {
		o.base = oidc.ClientContext(ctx, cfg.HTTPClient)
	}
	_, err := o.discover()
	return o, err
}

func (o *OIDC) discover() (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier != nil {
		return o.verifier, nil
	}
	if !o.lastTry.IsZero() && time.Since(o.lastTry) < discoveryBackoff {
		return nil, ErrProviderUnavailable
	}
	o.lastTry = time.Now()
	ctx, cancel := context.WithTimeout(o.base, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, o.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: discovering %s: %w", ErrProviderUnavailable, o.cfg.Issuer, err)
	}
	o.provider = provider
	o.verifier = provider.VerifierContext(o.base, &oidc.Config{ClientID: o.cfg.ClientID})
	return o.verifier, nil
}

// Provider returns the discovered provider, for the browser login flow.
func (o *OIDC) Provider() (*oidc.Provider, error) {
	if _, err := o.discover(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.provider, nil
}

// Verify implements TokenVerifier.
func (o *OIDC) Verify(ctx context.Context, raw string) (Principal, error) {
	v, err := o.discover()
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("%w: reading claims: %w", ErrUnauthorized, err)
	}
	name, _ := claims[o.cfg.UsernameClaim].(string)
	if name == "" {
		name = tok.Subject
	}
	return Principal{
		Subject: "user:" + name,
		Kind:    KindUser,
		Groups:  stringsClaim(claims[o.cfg.GroupsClaim]),
	}, nil
}

// stringsClaim reads a claim that providers spell as a list of strings or,
// for a single value, a bare string.
func stringsClaim(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
