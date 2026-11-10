package scan

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrepareCacheDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply")
	}
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
		wantDir bool
	}{
		{name: "CreatesMissingDirectoryPrivate", setup: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "a", "b")
		}, wantDir: true},
		{name: "AcceptsOwnPrivateDirectory", setup: func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "c")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return d
		}, wantDir: true},
		{name: "RefusesSymlink", setup: func(t *testing.T) string {
			root := t.TempDir()
			target := filepath.Join(root, "real")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, wantErr: true},
		{name: "RefusesGroupWritable", setup: func(t *testing.T) string {
			return looseDir(t, 0o770)
		}, wantErr: true},
		{name: "RefusesWorldWritable", setup: func(t *testing.T) string {
			return looseDir(t, 0o707)
		}, wantErr: true},
		{name: "RefusesAFile", setup: func(t *testing.T) string {
			f := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return f
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			err := PrepareCacheDir(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("PrepareCacheDir accepted an unsafe directory")
				}
				return
			}
			if err != nil {
				t.Fatalf("PrepareCacheDir: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				t.Fatalf("directory = %v, %v; want a private directory", info, err)
			}
		})
	}
}

func TestPrepareCacheDir_UnsafeErrorsAreIdentifiable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply")
	}
	err := PrepareCacheDir(looseDir(t, 0o770))
	if !errors.Is(err, ErrUnsafeCacheDir) {
		t.Fatalf("err = %v, want ErrUnsafeCacheDir", err)
	}
}

// looseDir makes a directory with exactly mode, whatever the umask.
func looseDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil { //nolint:gosec // the test needs a loose directory to prove it is refused
		t.Fatal(err)
	}
	return d
}
