package pypi

import (
	"errors"
	"html"
	"net/http"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
)

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	// Escape so a registry error string cannot inject HTML if a client
	// renders the body as a document.
	_, _ = w.Write([]byte(html.EscapeString(message) + "\n"))
}

func mapRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resolve.ErrUpstreamUnavailable):
		// Checked first: upstream bytes failing verification are the
		// upstream's fault, not a malformed client request.
		writeError(w, http.StatusBadGateway, "upstream index unavailable")
	case errors.Is(err, registry.ErrNotFound),
		errors.Is(err, registry.ErrWrongProtocol),
		errors.Is(err, registry.ErrNotPlaced):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, registry.ErrReadOnly):
		writeError(w, http.StatusMethodNotAllowed, "repository does not accept uploads")
	case errors.Is(err, registry.ErrFileExists):
		// PyPI answers 400 here; 409 says the same thing without pretending
		// the request was malformed, and twine surfaces both.
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, registry.ErrInvalidInput),
		errors.Is(err, cas.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
