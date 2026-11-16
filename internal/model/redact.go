package model

import "strings"

// Redacted is what is printed in place of a secret.
const Redacted = "<redacted>"

// RedactUserinfo replaces the credentials written into a URL or an image
// reference, as in https://user:pass@host or user:pass@registry/path. The
// whole userinfo goes, not only the password: a token is often written where
// the user name belongs. A value without credentials is returned unchanged.
func RedactUserinfo(s string) string {
	start := 0
	if i := strings.Index(s, "://"); i >= 0 {
		start = i + len("://")
	}
	// The authority ends at the first path, query, or fragment character, so
	// the @ of an image digest (registry/name@sha256:...) is never taken for
	// userinfo.
	end := len(s)
	if i := strings.IndexAny(s[start:], "/?#"); i >= 0 {
		end = start + i
	}
	at := strings.LastIndex(s[start:end], "@")
	if at < 0 {
		return s
	}
	return s[:start] + Redacted + s[start+at:]
}
