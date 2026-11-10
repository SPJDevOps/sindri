// Package scanjob is the queue contract between a publish and the scanner: the
// job kind, the creating actor and the dedupe key. It is a leaf so that
// internal/registry can enqueue a scan job and internal/scan can handle it
// without either importing the other.
package scanjob

// JobKind is the queue job kind of a scan.
const JobKind = "scan"

// Actor is the identity recorded as the creator of scan jobs and of the
// evidence the scanner attaches.
const Actor = "system:scanner"

// DedupeKey returns the queue dedupe key for scanning the subject with the
// given digest string.
func DedupeKey(digest string) string { return "scan:" + digest }
