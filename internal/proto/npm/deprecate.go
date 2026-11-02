package npm

// npm deprecate GETs the packument and PUTs it back without attachments. Of that
// document Sindri reads one field per version, and ignores the rest, so a
// deprecate cannot change which bytes an install resolves (ADR 24).

import (
	"encoding/json"
	"errors"
	"net/http"
)

// maxDeprecationBytes bounds one deprecation message.
const maxDeprecationBytes = 4 << 10

// handleDeprecate applies the deprecated fields of a publish-shaped body that
// holds no attachment. The caller has authorized the write on a hosted
// repository and validated nothing of the body.
func (h *handler) handleDeprecate(w http.ResponseWriter, r *http.Request, rt route, metadata []byte) {
	messages, err := readDeprecations(rt.pkg, metadata)
	if err != nil {
		// The JSON error is dropped because it can quote the document.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.reg.SetDeprecations(r.Context(), rt.repo, rt.pkg, messages); err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// readDeprecations returns the message for each version the body lists: the
// deprecated field, or "" when the version has none, which clears it. The top
// level is read as handlePublish reads it. The errors never quote the document.
func readDeprecations(pkg string, metadata []byte) (map[string]string, error) {
	top, err := decodeTopLevel(metadata)
	if err != nil {
		return nil, errors.New("deprecate body is not a JSON object")
	}
	var name string
	var attachments map[string]json.RawMessage
	var versions map[string]json.RawMessage
	if decodeMember(top["name"], &name) != nil ||
		decodeMember(top["_attachments"], &attachments) != nil ||
		decodeMember(top["versions"], &versions) != nil {
		return nil, errors.New("deprecate body has a field of the wrong type")
	}
	switch {
	case name != pkg:
		return nil, errors.New("deprecate body names a different package than the path")
	case len(attachments) != 0:
		return nil, errors.New("deprecate body must hold no attachment")
	case len(versions) == 0:
		return nil, errors.New("deprecate body lists no versions")
	}

	messages := make(map[string]string, len(versions))
	for version, raw := range versions {
		var entry map[string]json.RawMessage
		var message *string
		if json.Unmarshal(raw, &entry) != nil || decodeMember(entry["deprecated"], &message) != nil {
			return nil, errors.New("deprecate body has a version entry of the wrong type")
		}
		if message == nil {
			messages[version] = ""
			continue
		}
		if len(*message) > maxDeprecationBytes {
			return nil, errors.New("a deprecation message is too long")
		}
		messages[version] = *message
	}
	return messages, nil
}
