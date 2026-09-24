package model

import "time"

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
