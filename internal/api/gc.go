package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/queue"
)

type candidateResponse struct {
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	LastSeenAt string `json:"last_seen_at"`
}

type jobResponse struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	CreatedBy   string          `json:"created_by"`
	CreatedAt   string          `json:"created_at"`
	StartedAt   string          `json:"started_at,omitempty"`
	FinishedAt  string          `json:"finished_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

func jobJSON(j queue.Job) jobResponse {
	resp := jobResponse{
		ID:          j.ID,
		Kind:        j.Kind,
		State:       string(j.State),
		Attempts:    j.Attempts,
		MaxAttempts: j.MaxAttempts,
		CreatedBy:   j.CreatedBy,
		CreatedAt:   timestamp(j.CreatedAt),
		LastError:   j.LastError,
	}
	if j.StartedAt != nil {
		resp.StartedAt = timestamp(*j.StartedAt)
	}
	if j.FinishedAt != nil {
		resp.FinishedAt = timestamp(*j.FinishedAt)
	}
	if len(j.Result) > 0 {
		resp.Result = j.Result
	}
	return resp
}

// gcCandidates is the dry run: what a collection would delete now, in digest
// order, one page at a time. It reads and never writes.
func (a *api) gcCandidates(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "list garbage collection candidates") {
		return
	}
	limit, err := parseLimit(r)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	cands, next, err := a.gc.Candidates(r.Context(), r.URL.Query().Get("cursor"), int(limit))
	if err != nil {
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	resp := listResponse[candidateResponse]{Items: make([]candidateResponse, 0, len(cands)), NextCursor: next}
	for _, c := range cands {
		resp.Items = append(resp.Items, candidateResponse{
			Digest:     c.Digest.String(),
			Size:       c.Size,
			LastSeenAt: timestamp(c.LastSeenAt),
		})
	}
	a.writeJSON(w, r, http.StatusOK, resp)
}

// startGC enqueues a collection for the worker. A collection already queued
// or running is returned instead of a second one.
func (a *api) startGC(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "run garbage collection") {
		return
	}
	job, _, err := a.queue.Enqueue(r.Context(), queue.EnqueueParams{
		Kind:      gc.JobKind,
		DedupeKey: gc.JobKind,
		CreatedBy: actor(r),
	})
	if err != nil {
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10))
	a.writeJSON(w, r, http.StatusAccepted, jobJSON(job))
}

func (a *api) getJob(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, "read background jobs") {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "job id must be an integer")
		return
	}
	job, err := a.queue.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no job has this id")
			return
		}
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	a.writeJSON(w, r, http.StatusOK, jobJSON(job))
}
