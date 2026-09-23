package storage

import (
	"errors"

	"github.com/spj/sindri/internal/model"
)

var (
	// ErrNotFound is returned when a blob does not exist in the store.
	ErrNotFound = errors.New("blob not found")

	// ErrUploadNotFound is returned when a staged upload id is unknown, which
	// includes an upload that was already committed or aborted.
	ErrUploadNotFound = errors.New("upload not found")

	// ErrInvalidRange is returned when a requested range does not lie within
	// the blob.
	ErrInvalidRange = errors.New("invalid range")
)

// ErrInvalidDigest is returned when a zero or malformed digest reaches a store.
// Digests are parsed at the edge, so reaching a store with one is a bug rather
// than bad input. The sentinel belongs to the package that owns the concept, so
// this is an alias rather than a second error meaning the same thing.
var ErrInvalidDigest = model.ErrInvalidDigest
