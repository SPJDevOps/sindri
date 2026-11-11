package scan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrUnsafeCacheDir means Trivy's cache directory could be changed by
// someone other than this process's user. The vulnerability database lives
// there, and a poisoned one would make every scan report a clean result.
var ErrUnsafeCacheDir = errors.New("unsafe trivy cache directory")

// PrepareCacheDir creates the cache directory with mode 0700 and refuses it
// when it is a symbolic link, belongs to another user, or is writable by
// group or others. A directory that exists with a looser mode is refused, not
// repaired: the operator decides whether the earlier contents can be trusted.
func PrepareCacheDir(path string) error {
	// #nosec G703 -- the path is the operator's own scanning.cacheDir from the configuration file; this function exists to vet it.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("creating trivy cache directory %s: %w", path, err)
	}
	// #nosec G703 -- as above: an operator-configured path, checked right here.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspecting trivy cache directory %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafeCacheDir, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeCacheDir, path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s has mode %04o and is writable by group or others; run chmod 700", ErrUnsafeCacheDir, path, info.Mode().Perm())
	}
	if err := requireOwner(info); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafeCacheDir, path, err)
	}
	return nil
}
