package api

import (
	"encoding/json"
	"errors"
	"net/http"

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

// applyScope decides what the caller may apply. Phase C replaces this with
// the principal's administrator role and namespace grants.
func (a *api) applyScope(_ http.ResponseWriter, _ *http.Request) (reconcile.Scope, bool) {
	return reconcile.Scope{Global: true}, true
}
