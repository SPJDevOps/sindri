package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
)

// maxEvidenceBytes bounds one evidence document. SBOMs for large images run to
// tens of megabytes; anything bigger is not a document a gate needs to hold.
const maxEvidenceBytes = 64 << 20

// mountIdentity adds the subject, evidence, lifecycle, and decision routes.
func (a *api) mountIdentity(r chi.Router) {
	r.Get("/lifecycle/stages", a.listStages)
	r.Put("/lifecycle/stages", a.setStages)

	r.Route("/subjects/{digest}", func(r chi.Router) {
		r.Get("/", a.getSubject)
		r.Post("/evidence", a.attachEvidence)
		r.Get("/evidence", a.listEvidence)
		r.Get("/evidence/{id}", a.getEvidence)
		r.Post("/promote", a.promote)
		r.Post("/revoke", a.revoke)
		r.Get("/events", a.listEvents)
	})

	r.Get("/decisions", a.decide)
}

// actor names who made a change, for the ledger and evidence rows. It is the
// identity requireAuth accepted; M7 makes it a real user.
func actor(r *http.Request) string {
	if id, ok := r.Context().Value(identityCtxKey{}).(auth.Identity); ok && id.Subject != "" {
		return id.Subject
	}
	return "unknown"
}

// pathDigest parses the {digest} URL parameter at the edge, so the services
// below never see a malformed one.
func (a *api) pathDigest(w http.ResponseWriter, r *http.Request) (model.Digest, bool) {
	d, err := model.ParseDigest(chi.URLParam(r, "digest"))
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_DIGEST", "digest must be algorithm:hex")
		return model.Digest{}, false
	}
	return d, true
}

type stageJSON struct {
	Name  string     `json:"name"`
	Gates []gateJSON `json:"gates"`
}

type gateJSON struct {
	Evidence string `json:"evidence"`
}

type stagesBody struct {
	Stages []stageJSON `json:"stages"`
}

func (a *api) listStages(w http.ResponseWriter, r *http.Request) {
	stages, err := a.lifecycle.ListStages(r.Context())
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, stagesResponse(stages))
}

func (a *api) setStages(w http.ResponseWriter, r *http.Request) {
	var body stagesBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be {\"stages\": [...]}")
		return
	}
	specs := make([]lifecycle.StageSpec, 0, len(body.Stages))
	for _, s := range body.Stages {
		spec := lifecycle.StageSpec{Name: s.Name}
		for _, g := range s.Gates {
			spec.Gates = append(spec.Gates, lifecycle.GateRule{Evidence: model.EvidenceType(g.Evidence)})
		}
		specs = append(specs, spec)
	}
	stages, err := a.lifecycle.SetStages(r.Context(), specs)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, stagesResponse(stages))
}

func stagesResponse(stages []lifecycle.Stage) stagesBody {
	out := stagesBody{Stages: make([]stageJSON, 0, len(stages))}
	for _, s := range stages {
		js := stageJSON{Name: s.Name, Gates: make([]gateJSON, 0, len(s.Gates))}
		for _, g := range s.Gates {
			js.Gates = append(js.Gates, gateJSON{Evidence: string(g.Evidence)})
		}
		out.Stages = append(out.Stages, js)
	}
	return out
}

type subjectResponse struct {
	Digest     string               `json:"digest"`
	Kind       string               `json:"kind"`
	Size       int64                `json:"size"`
	CreatedAt  string               `json:"created_at"`
	Owners     []string             `json:"owners"`
	Provenance []provenanceResponse `json:"provenance"`
	States     []stateResponse      `json:"states"`
}

type provenanceResponse struct {
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref,omitempty"`
	BuildRef   string `json:"build_ref,omitempty"`
	RecordedAt string `json:"recorded_at"`
}

type stateResponse struct {
	Repository string `json:"repository"`
	Stage      string `json:"stage"`
	Action     string `json:"action"`
	Actor      string `json:"actor"`
	At         string `json:"at"`
}

func (a *api) getSubject(w http.ResponseWriter, r *http.Request) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	s, err := a.lifecycle.GetSubject(r.Context(), digest)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	resp := subjectResponse{
		Digest:     s.Digest.String(),
		Kind:       string(s.Kind),
		Size:       s.Size,
		CreatedAt:  timestamp(s.CreatedAt),
		Owners:     s.Owners,
		Provenance: make([]provenanceResponse, 0, len(s.Provenance)),
		States:     make([]stateResponse, 0, len(s.States)),
	}
	for _, p := range s.Provenance {
		resp.Provenance = append(resp.Provenance, provenanceResponse{
			SourceType: p.SourceType,
			SourceRef:  p.SourceRef,
			BuildRef:   p.BuildRef,
			RecordedAt: timestamp(p.RecordedAt),
		})
	}
	for _, st := range s.States {
		resp.States = append(resp.States, stateResponse{
			Repository: st.Repository,
			Stage:      st.Stage,
			Action:     string(st.Action),
			Actor:      st.Actor,
			At:         timestamp(st.At),
		})
	}
	a.writeJSON(w, r, http.StatusOK, resp)
}

type evidenceResponse struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

func evidenceJSON(ev evidence.Evidence) evidenceResponse {
	return evidenceResponse{
		ID:        ev.ID,
		Type:      string(ev.Type),
		MediaType: ev.MediaType,
		Digest:    ev.Digest.String(),
		Size:      ev.Size,
		CreatedAt: timestamp(ev.CreatedAt),
		CreatedBy: ev.CreatedBy,
	}
}

func (a *api) attachEvidence(w http.ResponseWriter, r *http.Request) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	size := int64(-1)
	if r.ContentLength > maxEvidenceBytes {
		a.writeError(w, r, http.StatusRequestEntityTooLarge, "TOO_LARGE", "evidence document is too large")
		return
	}
	if r.ContentLength >= 0 {
		size = r.ContentLength
	}
	ev, err := a.evidence.Attach(r.Context(), evidence.AttachParams{
		Subject:   digest,
		Type:      model.EvidenceType(r.URL.Query().Get("type")),
		MediaType: r.Header.Get("Content-Type"),
		Body:      http.MaxBytesReader(w, r.Body, maxEvidenceBytes),
		Size:      size,
		Actor:     actor(r),
	})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.writeError(w, r, http.StatusRequestEntityTooLarge, "TOO_LARGE", "evidence document is too large")
			return
		}
		a.writeLifecycleError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, evidenceJSON(ev))
}

func (a *api) listEvidence(w http.ResponseWriter, r *http.Request) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	list, err := a.evidence.List(r.Context(), digest)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	items := make([]evidenceResponse, 0, len(list))
	for _, ev := range list {
		items = append(items, evidenceJSON(ev))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[evidenceResponse]{Items: items})
}

func (a *api) getEvidence(w http.ResponseWriter, r *http.Request) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	ev, rc, err := a.evidence.Open(r.Context(), digest, chi.URLParam(r, "id"))
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", ev.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(ev.Size, 10))
	w.Header().Set("X-Sindri-Digest", ev.Digest.String())
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are sent; cut the connection so a short body is not
		// mistaken for a complete document (ADR 14).
		panic(http.ErrAbortHandler)
	}
}

type changeBody struct {
	Repository string `json:"repository"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
}

type eventResponse struct {
	ID         int64  `json:"id"`
	Digest     string `json:"digest"`
	Repository string `json:"repository"`
	Stage      string `json:"stage"`
	Action     string `json:"action"`
	Actor      string `json:"actor"`
	Reason     string `json:"reason,omitempty"`
	At         string `json:"at"`
}

func eventJSON(ev lifecycle.Event) eventResponse {
	return eventResponse{
		ID:         ev.ID,
		Digest:     ev.Digest.String(),
		Repository: ev.Repository,
		Stage:      ev.Stage,
		Action:     string(ev.Action),
		Actor:      ev.Actor,
		Reason:     ev.Reason,
		At:         timestamp(ev.At),
	}
}

func (a *api) promote(w http.ResponseWriter, r *http.Request) {
	a.change(w, r, a.lifecycle.Promote)
}

func (a *api) revoke(w http.ResponseWriter, r *http.Request) {
	a.change(w, r, a.lifecycle.Revoke)
}

func (a *api) change(w http.ResponseWriter, r *http.Request, apply func(context.Context, lifecycle.ChangeParams) (lifecycle.Event, error)) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	var body changeBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "request body must be {\"repository\", \"stage\", \"reason\"}")
		return
	}
	ev, err := apply(r.Context(), lifecycle.ChangeParams{
		Digest:     digest,
		Repository: body.Repository,
		Stage:      body.Stage,
		Actor:      actor(r),
		Reason:     body.Reason,
	})
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, eventJSON(ev))
}

func (a *api) listEvents(w http.ResponseWriter, r *http.Request) {
	digest, ok := a.pathDigest(w, r)
	if !ok {
		return
	}
	events, err := a.lifecycle.Events(r.Context(), digest)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	items := make([]eventResponse, 0, len(events))
	for _, ev := range events {
		items = append(items, eventJSON(ev))
	}
	a.writeJSON(w, r, http.StatusOK, listResponse[eventResponse]{Items: items})
}

type decisionResponse struct {
	Digest      string             `json:"digest"`
	Environment string             `json:"environment"`
	Decision    string             `json:"decision"`
	Reason      string             `json:"reason"`
	Approvals   []approvalResponse `json:"approvals"`
}

type approvalResponse struct {
	Repository string `json:"repository"`
	Actor      string `json:"actor"`
	Reason     string `json:"reason,omitempty"`
	At         string `json:"at"`
}

// decide answers "is exactly this digest approved for this environment?" It
// always answers 200 with approved or denied for a well-formed question, so
// an admission controller never has to interpret a 404 as a policy decision.
func (a *api) decide(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	digest, err := model.ParseDigest(query.Get("digest"))
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_DIGEST", "digest must be algorithm:hex")
		return
	}
	environment := query.Get("environment")
	if environment == "" {
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "environment is required")
		return
	}
	d, err := a.lifecycle.Decide(r.Context(), digest, environment)
	if err != nil {
		a.writeLifecycleError(w, r, err)
		return
	}
	resp := decisionResponse{
		Digest:      d.Digest.String(),
		Environment: d.Environment,
		Decision:    "denied",
		Reason:      d.Reason,
		Approvals:   make([]approvalResponse, 0, len(d.Approvals)),
	}
	if d.Approved {
		resp.Decision = "approved"
	}
	for _, ap := range d.Approvals {
		resp.Approvals = append(resp.Approvals, approvalResponse{
			Repository: ap.Repository,
			Actor:      ap.Actor,
			Reason:     ap.Reason,
			At:         timestamp(ap.At),
		})
	}
	a.writeJSON(w, r, http.StatusOK, resp)
}

func (a *api) writeLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	var gate *lifecycle.GateError
	switch {
	case errors.As(err, &gate):
		a.writeJSON(w, r, http.StatusConflict, ErrorBody{
			Code:      "GATE_FAILED",
			Message:   gate.Error(),
			RequestID: a.requestID(r.Context()),
			Missing:   gate.Missing,
		})
	case errors.Is(err, lifecycle.ErrSubjectNotFound), errors.Is(err, evidence.ErrSubjectNotFound):
		a.writeError(w, r, http.StatusNotFound, "SUBJECT_NOT_FOUND", "no hosted artifact has this digest")
	case errors.Is(err, lifecycle.ErrRepositoryNotFound):
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, lifecycle.ErrNotPlaced):
		a.writeError(w, r, http.StatusNotFound, "NOT_PLACED", err.Error())
	case errors.Is(err, evidence.ErrNotFound):
		a.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "evidence not found")
	case errors.Is(err, lifecycle.ErrUnknownStage):
		a.writeError(w, r, http.StatusBadRequest, "UNKNOWN_STAGE", err.Error())
	case errors.Is(err, lifecycle.ErrNotPromotable):
		a.writeError(w, r, http.StatusConflict, "NOT_PROMOTABLE", err.Error())
	case errors.Is(err, lifecycle.ErrNotApproved):
		a.writeError(w, r, http.StatusConflict, "NOT_APPROVED", err.Error())
	case errors.Is(err, lifecycle.ErrStageInUse):
		a.writeError(w, r, http.StatusConflict, "STAGE_IN_USE", err.Error())
	case errors.Is(err, lifecycle.ErrInvalidInput), errors.Is(err, evidence.ErrInvalidInput):
		a.writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		a.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
