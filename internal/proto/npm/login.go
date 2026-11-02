package npm

// npm login and whoami. Login verifies a credential and issues nothing: the
// password is an existing token, checked and handed back for npm to store as
// _authToken (ADR 24). The body holds a secret, so nothing here logs or echoes
// it (security rule 4).

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spj/sindri/internal/auth"
)

// maxLoginBody bounds a login document: a name and a token.
const maxLoginBody = 64 << 10

// handleUserLogin answers PUT -/user/org.couchdb.user:{name}. npm sends no
// credential header here, so the answer is the same for every repository,
// including one that does not exist: it neither loads nor mentions it. The
// username is ignored, as it is for Basic auth.
func (h *handler) handleUserLogin(w http.ResponseWriter, r *http.Request, rt route) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var login struct {
		Password string `json:"password"`
	}
	var tooLarge *http.MaxBytesError
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody)).Decode(&login)
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "login body is too large")
		return
	case err != nil:
		// The JSON error is dropped because it can quote the body.
		writeError(w, http.StatusBadRequest, "login body must be a JSON object")
		return
	}
	// Every verifier failure is the same answer, so the response does not say
	// which kind of token was tried or why it failed.
	if login.Password == "" {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if _, err := h.verifier.Verify(r.Context(), login.Password); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":    true,
		"id":    userDocPrefix + rt.user,
		"token": login.Password,
	})
}

// handleWebLogin answers -/v1/login, which npm tries before it falls back to
// the password prompt. Sindri has no browser login, so it is a constant 404 to
// every caller, method, and repository.
func (h *handler) handleWebLogin(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// handleWhoami answers GET -/whoami with the caller's subject. It authorizes
// like a read, so a caller who may not read the repository is told it does not
// exist.
func (h *handler) handleWhoami(w http.ResponseWriter, r *http.Request, rt route) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorize(w, r, rt.repo, auth.RoleRead) {
		return
	}
	if _, err := h.reg.RequireNPMRepository(r.Context(), rt.repo); err != nil {
		mapRegistryError(w, err)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"username": p.Subject})
}
