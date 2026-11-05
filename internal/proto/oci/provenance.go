package oci

import (
	"encoding/json"
	"net/http"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
)

// pushProvenance reads the publisher's provenance claim for a manifest push.
// The standard OCI annotations are the push-time metadata a build already
// stamps; the X-Sindri-* headers override them for a pipeline that wants to
// state its build reference explicitly. Neither is verified (ADR 16). A header
// the database cannot store is an error, returned before the manifest is read.
func pushProvenance(header http.Header, body []byte) (model.Provenance, error) {
	prov := model.Provenance{SourceType: model.ProvenanceOCIPush}
	claimed, err := model.HeaderProvenance(model.ProvenanceOCIPush, header.Get)
	if err != nil {
		return model.Provenance{}, err
	}

	var doc struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		prov.SourceRef = strings.TrimSpace(doc.Annotations[ocispec.AnnotationSource])
		prov.BuildRef = strings.TrimSpace(doc.Annotations[ocispec.AnnotationRevision])
	}
	if claimed.SourceRef != "" {
		prov.SourceRef = claimed.SourceRef
	}
	if claimed.BuildRef != "" {
		prov.BuildRef = claimed.BuildRef
	}
	return prov, nil
}
