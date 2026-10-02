package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/auth"
)

type tokenRequest struct {
	Name       string     `json:"name"`
	Repository string     `json:"repository"`
	Namespace  string     `json:"namespace"`
	Role       string     `json:"role"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

type issuedToken struct {
	auth.Token
	// Secret is shown once. Only its hash is stored.
	Secret string `json:"token"`
}

func (a *api) createToken(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "issue machine tokens") {
		return
	}
	var body tokenRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be {\"name\", \"role\", \"repository\" or \"namespace\"}")
		return
	}
	tok, secret, err := a.tokens.Issue(r.Context(), auth.IssueParams{
		Name:       body.Name,
		Repository: body.Repository,
		Namespace:  body.Namespace,
		Role:       auth.Role(body.Role),
		CreatedBy:  actor(r),
		ExpiresAt:  body.ExpiresAt,
	})
	if err != nil {
		a.writeTokenError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, issuedToken{Token: tok, Secret: secret})
}

func (a *api) listTokens(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "list machine tokens") {
		return
	}
	list, err := a.tokens.List(r.Context())
	if err != nil {
		a.writeTokenError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[auth.Token]{Items: list})
}

func (a *api) revokeToken(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "revoke machine tokens") {
		return
	}
	if err := a.tokens.Revoke(r.Context(), chi.URLParam(r, "id")); err != nil {
		a.writeTokenError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) writeTokenError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, auth.ErrTokenNotFound):
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "machine token not found")
	default:
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
