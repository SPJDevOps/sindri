// Package scan produces vulnerability scans and SBOMs for hosted artifacts
// (ADR 23, ADR 29). This file holds only the names that the publish path and
// the worker must agree on, so that internal/registry can enqueue a scan job
// without importing the scanner.
package scan

// JobKind is the queue job kind of a scan.
const JobKind = "scan"

// Actor is the identity recorded as the creator of scan jobs and of the
// evidence the scanner attaches.
const Actor = "system:scanner"

// DedupeKey returns the queue dedupe key for scanning the subject with the
// given digest string.
func DedupeKey(digest string) string { return "scan:" + digest }
