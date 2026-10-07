package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
)

// maxManifestBytes bounds one apply. Thousands of repositories fit well
// inside it; anything larger is not a manifest set.
const maxManifestBytes = 16 << 20

// configDiff reports how desired state differs from the database, changing
// nothing. It is a dry run of configApply, so the two cannot disagree.
func (a *api) configDiff(w http.ResponseWriter, r *http.Request) {
	a.reconcileRequest(w, r, true)
}

// configApply converges the database onto the posted desired state. This is
// the server half of `sindri apply -f <dir>`, the CI trigger of the one
// reconciler (ADR 7).
func (a *api) configApply(w http.ResponseWriter, r *http.Request) {
	a.reconcileRequest(w, r, false)
}

func (a *api) reconcileRequest(w http.ResponseWriter, r *http.Request, dryRun bool) {
	var m config.Manifests
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManifestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be the expanded manifests as JSON: "+err.Error())
		return
	}
	scope, ok := a.applyScope(w, r)
	if !ok {
		return
	}
	res, err := a.reconciler.Reconcile(r.Context(), &m, reconcile.Options{
		Source: model.SourceApply,
		Scope:  scope,
		Prune:  r.URL.Query().Get("prune") == "true",
		DryRun: dryRun,
	})
	if err != nil {
		a.writeReconcileError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, res)
}

func (a *api) writeReconcileError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "INTERNAL"
	switch {
	case errors.Is(err, reconcile.ErrLocked):
		status, code = http.StatusForbidden, "LOCKED"
	case errors.Is(err, reconcile.ErrOutOfScope):
		status, code = http.StatusForbidden, "FORBIDDEN"
	case errors.Is(err, reconcile.ErrInvalid):
		status, code = http.StatusBadRequest, "INVALID_CONFIGURATION"
	case errors.Is(err, registry.ErrManagedElsewhere):
		status, code = http.StatusConflict, "MANAGED_ELSEWHERE"
	case errors.Is(err, registry.ErrImmutableField):
		status, code = http.StatusConflict, "IMMUTABLE_FIELD"
	case errors.Is(err, registry.ErrHasHistory):
		status, code = http.StatusConflict, "HAS_HISTORY"
	case errors.Is(err, lifecycle.ErrStageInUse):
		status, code = http.StatusConflict, "STAGE_IN_USE"
	case errors.Is(err, lifecycle.ErrInvalidInput), errors.Is(err, registry.ErrInvalidInput):
		status, code = http.StatusBadRequest, "INVALID_ARGUMENT"
	}
	if status == http.StatusInternalServerError {
		a.writeError(w, r, status, code, "internal error")
		return
	}
	body := ErrorBody{Code: code, Message: err.Error(), RequestID: a.requestID(r.Context())}
	if errors.Is(err, reconcile.ErrInvalid) || errors.Is(err, reconcile.ErrLocked) || errors.Is(err, reconcile.ErrOutOfScope) {
		body.Details = config.AsErrors(err)
	}
	a.writeJSON(w, r, status, body)
}

// applyScope decides what the caller may apply. An administrator may apply
// anything. A namespace administrator, named by the stored OrgPolicy or holding
// a namespace-scoped admin token, may apply only that namespace's
// repositories. Anyone else is refused before any validation runs.
func (a *api) applyScope(w http.ResponseWriter, r *http.Request) (reconcile.Scope, bool) {
	p := principal(r)
	if auth.AuthorizeGlobal(p, "apply configuration") == nil {
		return reconcile.Scope{Global: true}, true
	}
	var namespaces []string
	if t := p.Token; t != nil && t.Role == auth.RoleAdmin && t.Namespace != "" {
		namespaces = append(namespaces, t.Namespace)
	}
	if p.Kind == auth.KindUser {
		policy, err := a.reconciler.StoredPolicy(r.Context())
		if err != nil {
			a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
			return reconcile.Scope{}, false
		}
		if policy != nil {
			for _, ns := range policy.Namespaces {
				if p.InGroup(ns.Admins) {
					namespaces = append(namespaces, ns.Name)
				}
			}
		}
	}
	if len(namespaces) == 0 {
		a.writeError(w, r, http.StatusForbidden, "FORBIDDEN", p.Subject+" may not apply configuration: it needs the admin role or a namespace the OrgPolicy delegates to it")
		return reconcile.Scope{}, false
	}
	return reconcile.Scope{Namespaces: namespaces}, true
}
