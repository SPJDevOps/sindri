// Package oci is the OCI distribution-spec adapter mounted at /v2.
//
// Handlers translate the wire protocol into registry and cas calls. Error
// bodies use the distribution-spec shape; the management API's JSON error
// model must never leak onto these routes.
package oci

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/storage"
)

const apiVersion = "registry/2.0"

// OCI error codes from the distribution specification.
const (
	CodeBlobUnknown       = "BLOB_UNKNOWN"
	CodeBlobUploadUnknown = "BLOB_UPLOAD_UNKNOWN"
	CodeDigestInvalid     = "DIGEST_INVALID"
	CodeManifestInvalid   = "MANIFEST_INVALID"
	CodeManifestUnknown   = "MANIFEST_UNKNOWN"
	CodeNameUnknown       = "NAME_UNKNOWN"
	CodeSizeInvalid       = "SIZE_INVALID"
	CodeUnsupported       = "UNSUPPORTED"
	CodeUnknown           = "UNKNOWN"
)

// errorResponse is the OCI distribution error envelope.
type errorResponse struct {
	Errors []errorDetail `json:"errors"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", apiVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Errors: []errorDetail{{Code: code, Message: message}},
	})
}

func mapRegistryError(w http.ResponseWriter, err error) {
	switch {
	// First: a bad upstream document can also wrap ErrInvalidInput, and it
	// is still the upstream's fault, not the client's.
	case errors.Is(err, resolve.ErrUpstreamUnavailable):
		// The distribution spec has no code for this; UNKNOWN is what other
		// registries send with a 502.
		writeError(w, http.StatusBadGateway, CodeUnknown, "upstream registry unavailable")
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrWrongProtocol):
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
	case errors.Is(err, registry.ErrNotPlaced):
		writeError(w, http.StatusNotFound, CodeBlobUnknown, "blob unknown to registry")
	case errors.Is(err, registry.ErrReadOnly):
		writeError(w, http.StatusMethodNotAllowed, CodeUnsupported, "repository does not accept pushes")
	case errors.Is(err, registry.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, CodeManifestInvalid, err.Error())
	case errors.Is(err, cas.ErrDigestMismatch), errors.Is(err, model.ErrInvalidDigest):
		writeError(w, http.StatusBadRequest, CodeDigestInvalid, err.Error())
	case errors.Is(err, cas.ErrSizeMismatch):
		writeError(w, http.StatusBadRequest, CodeSizeInvalid, err.Error())
	case errors.Is(err, storage.ErrUploadNotFound):
		writeError(w, http.StatusNotFound, CodeBlobUploadUnknown, "blob upload unknown to registry")
	default:
		writeError(w, http.StatusInternalServerError, CodeUnsupported, "internal error")
	}
}

func mapBlobReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resolve.ErrUpstreamUnavailable):
		mapRegistryError(w, err)
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrNotPlaced):
		writeError(w, http.StatusNotFound, CodeBlobUnknown, "blob unknown to registry")
	case errors.Is(err, registry.ErrWrongProtocol):
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
	default:
		mapRegistryError(w, err)
	}
}

func mapManifestReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resolve.ErrUpstreamUnavailable):
		mapRegistryError(w, err)
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrNotPlaced):
		writeError(w, http.StatusNotFound, CodeManifestUnknown, "manifest unknown")
	case errors.Is(err, registry.ErrWrongProtocol):
		writeError(w, http.StatusNotFound, CodeNameUnknown, "repository name not known to registry")
	case errors.Is(err, registry.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, CodeManifestInvalid, err.Error())
	default:
		mapRegistryError(w, err)
	}
}
