package npm

// The dist-tag routes under /-/package/{name}/dist-tags. A tag is a pointer to
// a version; the registry records each move (ADR 24).

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spj/sindri/internal/auth"
)

// maxDistTagBody bounds the JSON string naming a version.
const maxDistTagBody = 1 << 10

func (h *handler) handleDistTags(w http.ResponseWriter, r *http.Request, rt route) {
	switch {
	case r.Method == http.MethodGet && rt.tag == "":
		h.listDistTags(w, r, rt)
	case r.Method == http.MethodPut && rt.tag != "":
		h.setDistTag(w, r, rt)
	case r.Method == http.MethodDelete && rt.tag != "":
		h.removeDistTag(w, r, rt)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// listDistTags answers with what readers see, including the latest fallback.
func (h *handler) listDistTags(w http.ResponseWriter, r *http.Request, rt route) {
	if !h.authorizeHostedRead(w, r, rt) {
		return
	}
	pkg, err := h.reg.GetNPMPackage(r.Context(), rt.repo, rt.pkg)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, effectiveDistTags(pkg.Versions, pkg.DistTags))
}

// authorizeDistTagWrite also settles the names: a package name that could not
// exist is the same 404 as one that does not, and a tag that is not one is a 400.
func (h *handler) authorizeDistTagWrite(w http.ResponseWriter, r *http.Request, rt route) bool {
	if !h.authorizeHostedWrite(w, r, rt) {
		return false
	}
	if validateName(rt.pkg) != nil {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	if !validTag(rt.tag) {
		writeError(w, http.StatusBadRequest, "dist-tag is not a valid tag name")
		return false
	}
	return true
}

func (h *handler) setDistTag(w http.ResponseWriter, r *http.Request, rt route) {
	if !h.authorizeDistTagWrite(w, r, rt) {
		return
	}
	var version string
	var tooLarge *http.MaxBytesError
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDistTagBody))
	err := dec.Decode(&version)
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "dist-tag body is too large")
		return
	case err != nil || dec.More():
		// The JSON error is dropped because it can quote the body.
		writeError(w, http.StatusBadRequest, "dist-tag body must be a JSON string naming a version")
		return
	}
	principal, _ := auth.PrincipalFromContext(r.Context())
	if err := h.reg.SetDistTag(r.Context(), rt.repo, rt.pkg, rt.tag, version, principal.Subject); err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (h *handler) removeDistTag(w http.ResponseWriter, r *http.Request, rt route) {
	if !h.authorizeDistTagWrite(w, r, rt) {
		return
	}
	principal, _ := auth.PrincipalFromContext(r.Context())
	if err := h.reg.RemoveDistTag(r.Context(), rt.repo, rt.pkg, rt.tag, principal.Subject); err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}
