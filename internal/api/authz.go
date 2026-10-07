package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// principal returns the caller auth.Middleware accepted. Every route sits
// behind it, so the zero value never authorizes anything.
func principal(r *http.Request) auth.Principal {
	p, _ := auth.PrincipalFromContext(r.Context())
	return p
}

// actor names who made a change, for the ledger and evidence rows.
func actor(r *http.Request) string {
	if p := principal(r); p.Subject != "" {
		return p.Subject
	}
	return "unknown"
}

// requireRepo loads a repository and authorizes the caller for need. A caller
// who may not read a repository is told it does not exist, so names are not
// disclosed to people who cannot see them; one who may read but not write is
// told so.
func (a *api) requireRepo(w http.ResponseWriter, r *http.Request, name string, need auth.Role) (model.Repository, bool) {
	repo, err := a.reg.GetRepository(r.Context(), name)
	if err != nil {
		a.writeRegistryError(w, r, err)
		return model.Repository{}, false
	}
	p := principal(r)
	if err := auth.Authorize(p, repo, need); err != nil {
		if need != auth.RoleRead && auth.Authorize(p, repo, auth.RoleRead) == nil {
			a.writeError(w, r, http.StatusForbidden, "FORBIDDEN", err.Error())
			return model.Repository{}, false
		}
		a.writeRegistryError(w, r, registry.ErrNotFound)
		return model.Repository{}, false
	}
	return repo, true
}

// requireAdmin authorizes an action that names no repository.
func (a *api) requireAdmin(w http.ResponseWriter, r *http.Request, what string) bool {
	if err := auth.AuthorizeGlobal(principal(r), what); err != nil {
		a.writeError(w, r, http.StatusForbidden, "FORBIDDEN", err.Error())
		return false
	}
	return true
}

// requireSubject authorizes access to a subject through the hosted
// repositories that own it: the caller needs need on at least one.
func (a *api) requireSubject(w http.ResponseWriter, r *http.Request, digest model.Digest, need auth.Role) bool {
	p := principal(r)
	if p.Admin {
		return true
	}
	s, err := a.lifecycle.GetSubject(r.Context(), digest)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return false
	}
	readable := false
	for _, owner := range s.Owners {
		name, ok := strings.CutPrefix(owner, registry.OwnerPrefix)
		if !ok {
			continue
		}
		repo, err := a.reg.GetRepository(r.Context(), name)
		if errors.Is(err, registry.ErrNotFound) {
			continue
		}
		if err != nil {
			a.writeRegistryError(w, r, err)
			return false
		}
		if auth.Authorize(p, repo, need) == nil {
			return true
		}
		readable = readable || auth.Authorize(p, repo, auth.RoleRead) == nil
	}
	if readable {
		a.writeError(w, r, http.StatusForbidden, "FORBIDDEN", "the caller may read this artifact but not change it")
		return false
	}
	a.writeError(w, r, http.StatusNotFound, "SUBJECT_NOT_FOUND", "no hosted artifact has this digest")
	return false
}

// denyUnauthenticated is the management API's refusal for a request with no
// acceptable credential.
func (a *api) denyUnauthenticated(w http.ResponseWriter, r *http.Request, _ error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="sindri"`)
	a.writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "a bearer token is required")
}
