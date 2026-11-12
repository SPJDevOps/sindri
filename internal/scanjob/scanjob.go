// Package scanjob is the queue contract between a publish and the scanner: the
// job kind, the creating actor and the dedupe key. It is a leaf so that
// internal/registry can enqueue a scan job and internal/scan can handle it
// without either importing the other.
package scanjob

// JobKind is the queue job kind of a scan.
const JobKind = "scan"

// RescanJobKind is the queue job kind of the schedule that fans out one scan
// job per approved subject (ADR 31). Its dedupe key is its kind, like the
// other schedules.
const RescanJobKind = "rescan_approved"

// Actor is the identity recorded as the creator of scan jobs and of the
// evidence the scanner attaches.
const Actor = "system:scanner"

// DedupeKey returns the queue dedupe key for scanning the subject with the
// given digest string.
func DedupeKey(digest string) string { return "scan:" + digest }

// Args are the arguments of a scan job. Repository is the hosted repository
// that published the subject: the scanner reads only blobs that repository
// holds (invariant I2). A job without one, enqueued before this field
// existed, is skipped rather than scanned unchecked.
type Args struct {
	Digest     string `json:"digest"`
	Kind       string `json:"kind"`
	Repository string `json:"repository,omitempty"`
}
