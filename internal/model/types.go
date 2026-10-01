package model

import (
	"strings"
	"time"
)

// RepoType names how a repository participates in resolution.
type RepoType string

const (
	// RepoTypeHosted holds first-party artifacts.
	RepoTypeHosted RepoType = "hosted"
	// RepoTypeProxy mirrors an upstream on miss.
	RepoTypeProxy RepoType = "proxy"
	// RepoTypeVirtual resolves an ordered member list.
	RepoTypeVirtual RepoType = "virtual"
)

// Protocol names a package ecosystem.
type Protocol string

const (
	// ProtocolOCI is the OCI distribution protocol.
	ProtocolOCI Protocol = "oci"
	// ProtocolPyPI is the Python package index protocol.
	ProtocolPyPI Protocol = "pypi"
)

// ManagedBy records whether a repository row is owned by config reconcile or
// the management API. The two must never fight over the same row (M7).
type ManagedBy string

const (
	// ManagedByAPI means the row was created through /api/v1.
	ManagedByAPI ManagedBy = "api"
	// ManagedByConfig means the row is owned by the reconciler.
	ManagedByConfig ManagedBy = "config"
)

// Repository is a named placement namespace.
type Repository struct {
	ID        string
	Name      string
	Type      RepoType
	Protocol  Protocol
	Locked    bool
	ManagedBy ManagedBy
	CreatedAt time.Time
}

// Package is a named artifact family inside a repository.
type Package struct {
	ID             string
	RepoID         string
	NormalizedName string
	DisplayName    string
}

// Version is one published version of a package.
type Version struct {
	ID        string
	PackageID string
	Version   string
}

// File is a named placement of a blob under a version.
type File struct {
	ID        string
	VersionID string
	Digest    Digest
	Filename  string
	MediaType string
	Size      int64
}

// SubjectKind classifies a subject by what its digest names.
type SubjectKind string

const (
	// SubjectKindOCIManifest is an OCI image manifest.
	SubjectKindOCIManifest SubjectKind = "oci-manifest"
	// SubjectKindOCIIndex is an OCI image index (multi-arch).
	SubjectKindOCIIndex SubjectKind = "oci-index"
	// SubjectKindPyPIDist is a wheel or sdist.
	SubjectKindPyPIDist SubjectKind = "pypi-dist"
	// SubjectKindFile is a file published through the management API.
	SubjectKindFile SubjectKind = "file"
)

// Provenance source types: which publish path recorded the claim.
const (
	ProvenanceOCIPush    = "oci-push"
	ProvenancePyPIUpload = "pypi-upload"
	ProvenanceAPIPublish = "api-publish"
)

// Headers a publisher may send to state where an artifact came from. They are
// recorded as claims; v1 does not verify them (ADR 16).
const (
	HeaderSourceRef = "X-Sindri-Source-Ref"
	HeaderBuildRef  = "X-Sindri-Build-Ref"
)

// Provenance is what a publisher stated about an artifact's origin at push
// time: the source repository and the build that produced it.
type Provenance struct {
	SourceType string
	SourceRef  string
	BuildRef   string
}

// HeaderProvenance builds a provenance claim from request headers. get is a
// header lookup such as http.Header.Get, taken as a function so this package
// stays free of the HTTP layer.
func HeaderProvenance(sourceType string, get func(string) string) Provenance {
	return Provenance{
		SourceType: sourceType,
		SourceRef:  strings.TrimSpace(get(HeaderSourceRef)),
		BuildRef:   strings.TrimSpace(get(HeaderBuildRef)),
	}
}

// EvidenceType names what an evidence document attests to. v1 stores and gates
// on the presence of a type; it does not interpret the document (ADR 16).
type EvidenceType string

const (
	// EvidenceSBOM is a software bill of materials.
	EvidenceSBOM EvidenceType = "sbom"
	// EvidenceSignature is a signature over the subject.
	EvidenceSignature EvidenceType = "signature"
	// EvidenceScan is a vulnerability scan report.
	EvidenceScan EvidenceType = "scan"
	// EvidenceAttestation is an in-toto or similar attestation.
	EvidenceAttestation EvidenceType = "attestation"
)

// Valid reports whether t is one of the evidence types v1 accepts.
func (t EvidenceType) Valid() bool {
	switch t {
	case EvidenceSBOM, EvidenceSignature, EvidenceScan, EvidenceAttestation:
		return true
	default:
		return false
	}
}
