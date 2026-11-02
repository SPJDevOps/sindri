package npm

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
)

// writeJSON answers with v as the JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is out; an encode failure is a closed connection.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers in the {"error": ...} shape npm prints to the user.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// mapRegistryError translates a registry or resolve error into the npm
// response. Only messages written for the client are returned; the 500 path
// returns a fixed string so no internal text leaks.
func mapRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resolve.ErrUpstreamUnavailable):
		// Checked first: upstream bytes failing verification are the
		// upstream's fault, not a malformed client request.
		writeError(w, http.StatusBadGateway, "upstream registry unavailable")
	case errors.Is(err, registry.ErrNotFound),
		errors.Is(err, registry.ErrWrongProtocol),
		errors.Is(err, registry.ErrNotPlaced):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, registry.ErrReadOnly):
		writeError(w, http.StatusMethodNotAllowed, "repository does not accept publishes")
	case errors.Is(err, registry.ErrVersionExists),
		errors.Is(err, registry.ErrFileExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, registry.ErrInvalidInput),
		errors.Is(err, cas.ErrDigestMismatch),
		errors.Is(err, errTarballRefused):
		// errUnpackedTooLarge wraps errTarballRefused.
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
