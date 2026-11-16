package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// blockedResponse is one block: the audit record of a digest an operator
// evicted from an OCI proxy repository and refuses to fetch again (SIN-73).
type blockedResponse struct {
	Digest    string    `json:"digest"`
	BlockedBy string    `json:"blockedBy"`
	Reason    string    `json:"reason"`
	BlockedAt time.Time `json:"blockedAt"`
}

func blockedJSON(b registry.BlockedDigest) blockedResponse {
	return blockedResponse{
		Digest:    b.Digest.String(),
		BlockedBy: b.BlockedBy,
		Reason:    b.Reason,
		BlockedAt: b.BlockedAt,
	}
}

// blockDigest evicts a digest from an OCI proxy repository and blocks it. It
// is idempotent: blocking a blocked digest answers 200 with the first record.
func (a *api) blockDigest(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "block digests") {
		return
	}
	digest, err := model.ParseDigest(chi.URLParam(r, "digest"))
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "digest is not valid: "+err.Error())
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be JSON with an optional reason")
		return
	}
	blocked, err := a.reg.EvictDigest(r.Context(), chi.URLParam(r, "repo"), digest, actor(r), body.Reason)
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, blockedJSON(blocked))
}

// unblockDigest lifts a block. The digest is not placed again; the next pull
// fetches it from upstream like any miss.
func (a *api) unblockDigest(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "unblock digests") {
		return
	}
	digest, err := model.ParseDigest(chi.URLParam(r, "digest"))
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "digest is not valid: "+err.Error())
		return
	}
	if err := a.reg.UnblockDigest(r.Context(), chi.URLParam(r, "repo"), digest); err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listBlocked(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "list blocked digests") {
		return
	}
	blocks, err := a.reg.ListBlockedDigests(r.Context(), chi.URLParam(r, "repo"))
	if err != nil {
		a.writeRegistryError(w, r, err)
		return
	}
	items := make([]blockedResponse, 0, len(blocks))
	for _, b := range blocks {
		items = append(items, blockedJSON(b))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[blockedResponse]{Items: items})
}
