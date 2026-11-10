//go:build !unix

package scan

import "io/fs"

// requireOwner has nothing to check where file ownership is not a uid.
func requireOwner(fs.FileInfo) error { return nil }
