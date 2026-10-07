package config

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrNoCredentials means a proxy names a credentials reference that is not
// defined. Validation refuses such a repository when it is written, so at pull
// time this means the section changed under a running server.
var ErrNoCredentials = errors.New("credentials reference not defined")

// CredentialStore serves the credentials section to the proxy client. It
// implements remote.Credentials, and Swap replaces the section atomically
// when SIGHUP reloads the file, so a pull in flight sees either the old
// secrets or the new ones, never a mix (ADR 15).
type CredentialStore struct {
	current atomic.Pointer[Credentials]
}

// NewCredentialStore returns a store holding cs.
func NewCredentialStore(cs Credentials) *CredentialStore {
	s := &CredentialStore{}
	s.Swap(cs)
	return s
}

// Swap replaces the credentials.
func (s *CredentialStore) Swap(cs Credentials) {
	cp := make(Credentials, len(cs))
	for k, v := range cs {
		cp[k] = v
	}
	s.current.Store(&cp)
}

// Names lists the defined references.
func (s *CredentialStore) Names() map[string]struct{} {
	return s.current.Load().Names()
}

// Lookup implements remote.Credentials.
func (s *CredentialStore) Lookup(_ context.Context, ref string) (username, password string, err error) {
	if ref == "" {
		return "", "", nil
	}
	c, ok := (*s.current.Load())[ref]
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrNoCredentials, ref)
	}
	return c.Username, c.Password, nil
}
