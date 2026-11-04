package npm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

const (
	// maxNameLength is npm's own limit.
	maxNameLength = 214
	// maxVersionLength is node-semver's limit, so a client could not resolve a
	// longer version anyway.
	maxVersionLength = 256
	// maxTagLength only has to stop a pathological tag reaching the database.
	maxTagLength = 256
)

var (
	// No part may start with a dot, which is also what keeps "." and ".." out.
	// An unscoped name may not start with an underscore either; npm applies that
	// rule to the whole name, so @acme/_internal and @_acme/foo are legal. Uppercase
	// and other punctuation are npm's allowance for names that predate its rules,
	// and hosted publishes are new.
	namePattern = regexp.MustCompile(`^(?:@[a-z0-9_-][a-z0-9._-]*/[a-z0-9_-][a-z0-9._-]*|[a-z0-9-][a-z0-9._-]*)$`)
	tagPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// readNamePattern is what may be asked for through a proxy: the characters
	// encodeURIComponent leaves alone, which is what npm has always required, in
	// either case. No part may start with a dot.
	readNamePattern = regexp.MustCompile(`^(?:@[A-Za-z0-9_~!'()*-][A-Za-z0-9._~!'()*-]*/)?[A-Za-z0-9_~!'()*-][A-Za-z0-9._~!'()*-]*$`)
)

// validateName applies npm's rules for a new package name. It is the write-time
// check that keeps the rest of the adapter from seeing a name that would be
// unreachable, or dangerous once it is a path in node_modules. The errors never
// quote the name, which comes straight from the client.
func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("package name is required")
	case len(name) > maxNameLength:
		return fmt.Errorf("package name is longer than %d bytes", maxNameLength)
	case !namePattern.MatchString(name):
		return errors.New("package name must be lowercase and URL-safe, optionally @scope/name, and not start with a dot, or an underscore unless scoped")
	case name == "node_modules", name == "favicon.ico":
		return errors.New("package name is reserved")
	case name == "-", strings.HasSuffix(name, "/-"):
		// A segment of "-" splits a route, so the tarball path could not be parsed.
		return errors.New("package name may not be a single hyphen")
	}
	return nil
}

// validReadName accepts a name a proxy may ask upstream for. Names from before
// npm's rules, like JSONStream, exist and must install, so it allows uppercase
// and old punctuation where validateName, the publish check, does not.
//
// It runs before any resolver or upstream call. The pattern is ASCII on purpose:
// PackageClaimed lowers only ASCII, so a name spelled with a Kelvin sign would
// read as unclaimed and could reach a proxy around a hosted claim on @Kit/*. It
// also keeps slashes, escapes, and control characters out of the upstream URL
// and the database, and a name of "-" could not be routed.
func validReadName(name string) bool {
	switch {
	case len(name) > maxNameLength, !readNamePattern.MatchString(name):
		return false
	case name == "node_modules", name == "favicon.ico":
		return false
	case name == "-", strings.HasSuffix(name, "/-"):
		return false
	}
	return true
}

// validVersion accepts a full semantic version. Comparing with the canonical
// form is what refuses the short forms semver.IsValid allows ("v1.0") and any
// build metadata, which npm drops and would give one version two spellings.
func validVersion(v string) bool {
	if len(v) > maxVersionLength {
		return false
	}
	return semver.Canonical("v"+v) == "v"+v
}

// validTag accepts a dist-tag. npm resolves foo@1.0.0 as a version before it
// looks for a tag, so a tag spelled like a version could never be selected.
func validTag(t string) bool {
	if len(t) > maxTagLength || t == "." || t == ".." || !tagPattern.MatchString(t) {
		return false
	}
	return !semver.IsValid(t) && !semver.IsValid("v"+t)
}

// tarballFilename builds the filename from name and version, never from the
// client: its attachment key holds a slash for a scoped name (ADR 24).
func tarballFilename(name, version string) string {
	base := name[strings.LastIndex(name, "/")+1:]
	return base + "-" + version + ".tgz"
}
