package auth

import (
	"fmt"
	"slices"

	"github.com/spj/sindri/internal/model"
)

// Authenticated is the grant every signed-in caller holds. There is no grant
// for anonymous callers in v1 (ADR 18).
const Authenticated = "authenticated"

// Authorize decides whether p may act on repo with the given role. It is pure
// and reads only what the repository row already carries, so a pull costs no
// lookup beyond the one the adapter makes to find the repository: the
// download path stays a lookup (ARCHITECTURE.md section 7).
//
// A read through a virtual repository is authorized against the virtual
// repository, because its access list is what the configuration's author
// wrote for that URL.
func Authorize(p Principal, repo model.Repository, need Role) error {
	if allowed(p, repo, need) {
		return nil
	}
	return fmt.Errorf("%w: %s may not %s %s", ErrForbidden, p.Subject, verb(need), repo.Name)
}

// AuthorizeGlobal decides an action that names no repository, such as
// creating one or changing the lifecycle stages. Only administrators hold it.
func AuthorizeGlobal(p Principal, what string) error {
	if p.Admin {
		return nil
	}
	if t := p.Token; t != nil && t.Role == RoleAdmin && t.Repository == "" && t.Namespace == "" {
		return nil
	}
	return fmt.Errorf("%w: %s may not %s; it needs the admin role", ErrForbidden, p.Subject, what)
}

func allowed(p Principal, repo model.Repository, need Role) bool {
	if !need.Valid() {
		return false
	}
	if p.Admin {
		return true
	}
	if t := p.Token; t != nil {
		return tokenAllows(*t, repo, need)
	}
	if p.Kind != KindUser {
		return false
	}
	switch need {
	case RoleRead:
		return grants(p, repo.Access.Read) || grants(p, repo.Access.Write)
	case RoleWrite:
		return grants(p, repo.Access.Write)
	default:
		return false
	}
}

func tokenAllows(t TokenScope, repo model.Repository, need Role) bool {
	if !t.Role.Includes(need) {
		return false
	}
	switch {
	case t.Repository != "":
		return repo.ID != "" && t.Repository == repo.Name
	case t.Namespace != "":
		return repo.ID != "" && repo.Namespace == t.Namespace
	default:
		return true
	}
}

func grants(p Principal, list []string) bool {
	if slices.Contains(list, Authenticated) {
		return true
	}
	return p.InGroup(list)
}

func verb(r Role) string {
	switch r {
	case RoleRead:
		return "read"
	case RoleWrite:
		return "write to"
	default:
		return "administer"
	}
}
