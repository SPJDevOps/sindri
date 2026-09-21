// Package version carries build metadata stamped in at link time.
package version

import (
	"fmt"
	"runtime"
)

// These are overridden at build time via -ldflags. See the Makefile.
var (
	// Version is the semantic version of this build.
	Version = "0.0.0-dev"
	// Commit is the git revision this binary was built from.
	Commit = "unknown"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// String renders a single-line human readable build identifier.
func String() string {
	return fmt.Sprintf("sindri %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
