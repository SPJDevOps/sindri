package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

const defaultAuditPage = 100

// readableRepositories returns the names and ids of the repositories the
// caller may read, or all == true for an administrator, who may read every
// repository including ones created after this call.
func (a *api) readableRepositories(r *http.Request) (all bool, names map[string]bool, ids []string, err error) {
	p := principal(r)
	if p.Admin {
		return true, nil, nil, nil
	}
	repos, err := a.reg.AllRepositories(r.Context())
	if err != nil {
		return false, nil, nil, err
	}
	names = map[string]bool{}
	ids = []string{}
	for _, repo := range repos {
		if auth.Authorize(p, repo, auth.RoleRead) == nil {
			names[repo.Name] = true
			ids = append(ids, repo.ID)
		}
	}
	return false, names, ids, nil
}

// audit serves GET /audit: the lifecycle ledger, newest first, limited to
// events in repositories the caller may read. Read access to a repository
// already shows its subjects' states, so its ledger reveals nothing more; an
// administrator-only trail would only push teams to scrape every subject.
func (a *api) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := lifecycle.AuditParams{
		Stage:  q.Get("stage"),
		Actor:  q.Get("actor"),
		Action: lifecycle.Action(q.Get("action")),
		Limit:  defaultAuditPage,
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > lifecycle.MaxAuditPage {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT",
				"limit must be an integer from 1 to "+strconv.Itoa(lifecycle.MaxAuditPage))
			return
		}
		params.Limit = n
	}
	if raw := q.Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "cursor must be the next_cursor of a previous page")
			return
		}
		params.Before = n
	}
	if raw := q.Get("digest"); raw != "" {
		d, err := model.ParseDigest(raw)
		if err != nil {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_DIGEST", "digest must be algorithm:hex")
			return
		}
		params.Digest = d
	}
	switch params.Action {
	case "", lifecycle.ActionApprove, lifecycle.ActionRevoke:
	default:
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "action must be approve or revoke")
		return
	}
	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"since", &params.Since}, {"until", &params.Until}} {
		raw := q.Get(f.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", f.name+" must be an RFC 3339 time, for example 2026-10-01T00:00:00Z")
			return
		}
		*f.dst = t
	}

	if name := q.Get("repository"); name != "" {
		repo, ok := a.requireRepo(w, r, name, auth.RoleRead)
		if !ok {
			return
		}
		params.RepositoryIDs = []string{repo.ID}
	} else {
		all, _, ids, err := a.readableRepositories(r)
		if err != nil {
			a.writeRegistryError(w, r, err)
			return
		}
		if !all {
			params.RepositoryIDs = ids
		}
	}

	events, err := a.lifecycle.Audit(r.Context(), params)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	resp := listResponse[eventResponse]{Items: make([]eventResponse, 0, len(events))}
	for _, ev := range events {
		resp.Items = append(resp.Items, eventJSON(ev))
	}
	if len(events) == params.Limit {
		resp.NextCursor = strconv.FormatInt(events[len(events)-1].ID, 10)
	}
	a.writeJSON(w, r, http.StatusOK, resp)
}

// visibleOwners keeps the owners that name a repository the caller may read.
// A subject owned by two teams' repositories is visible to both teams, and
// neither learns the other's repository name from it.
func visibleOwners(owners []string, all bool, names map[string]bool) []string {
	if all {
		return owners
	}
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		if name, ok := strings.CutPrefix(o, registry.OwnerPrefix); ok && names[name] {
			out = append(out, o)
		}
	}
	return out
}
