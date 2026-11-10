//go:build unix

package scan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// requireOwner refuses a directory owned by another user.
func requireOwner(info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no owner information")
	}
	if uid := os.Geteuid(); int(st.Uid) != uid {
		return fmt.Errorf("is owned by uid %d, not by uid %d running this process", st.Uid, uid)
	}
	return nil
}
