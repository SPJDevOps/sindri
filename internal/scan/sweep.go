package scan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagingPrefix is the name prefix Handle gives each staging directory.
const stagingPrefix = "sindri-scan-"

// SweepResult reports what SweepStaging removed.
type SweepResult struct {
	Removed int   // directories removed
	Bytes   int64 // bytes of regular files they held
}

// SweepStaging removes sindri-scan-* directories directly under dir whose own
// modification time is more than olderThan ago. A killed process leaves its
// staging directory behind, and nothing else removes it. The job deadline
// bounds a live job's life, so a caller passing an olderThan beyond that
// deadline never removes a directory in use, even on a volume shared by
// several processes. Symbolic links and non-directories are never followed or
// removed. A missing dir is not an error. Per-entry failures do not stop the
// sweep; they are joined into the returned error.
func SweepStaging(dir string, olderThan time.Duration) (SweepResult, error) {
	var res SweepResult
	// #nosec G304 G703 -- dir is derived from the operator's scanning.cacheDir.
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("listing scan staging directory %s: %w", dir, err)
	}
	cutoff := time.Now().Add(-olderThan)
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("inspecting %s: %w", path, err))
			}
			continue
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.ModTime().After(cutoff) {
			continue
		}
		size := dirBytes(path)
		// #nosec G122 -- the directory is a sindri-scan-* child of the private staging directory, vetted by Lstat above.
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
			continue
		}
		res.Removed++
		res.Bytes += size
	}
	return res, errors.Join(errs...)
}

// dirBytes totals the regular files under root without following links. It is
// best effort: it feeds a log line only.
func dirBytes(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best effort, a vanished entry is not a failure
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
