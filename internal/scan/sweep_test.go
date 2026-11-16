package scan

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepStaging(t *testing.T) {
	age := func(t *testing.T, p string, d time.Duration) {
		t.Helper()
		when := time.Now().Add(-d)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(t *testing.T, root, name string, size int, old time.Duration) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if err := os.WriteFile(filepath.Join(p, "layer"), make([]byte, size), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		age(t, p, old)
		return p
	}
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}

	t.Run("OldStagingDirectoryIsRemovedAndCounted", func(t *testing.T) {
		root := t.TempDir()
		p := mk(t, root, "sindri-scan-123", 100, 3*time.Hour)
		res, err := SweepStaging(root, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if exists(p) || res.Removed != 1 || res.Bytes != 100 {
			t.Fatalf("exists=%v result=%+v, want removed 1 dir of 100 bytes", exists(p), res)
		}
	})
	t.Run("FreshStagingDirectoryIsKept", func(t *testing.T) {
		root := t.TempDir()
		p := mk(t, root, "sindri-scan-123", 10, time.Minute)
		res, err := SweepStaging(root, time.Hour)
		if err != nil || !exists(p) || res.Removed != 0 {
			t.Fatalf("err=%v exists=%v result=%+v", err, exists(p), res)
		}
	})
	t.Run("OtherNamesAreKept", func(t *testing.T) {
		root := t.TempDir()
		p := mk(t, root, "something-else", 10, 3*time.Hour)
		res, err := SweepStaging(root, time.Hour)
		if err != nil || !exists(p) || res.Removed != 0 {
			t.Fatalf("err=%v exists=%v result=%+v", err, exists(p), res)
		}
	})
	t.Run("SymlinksAndFilesAreNotFollowedOrRemoved", func(t *testing.T) {
		root := t.TempDir()
		target := mk(t, t.TempDir(), "sindri-scan-target", 10, 3*time.Hour)
		link := filepath.Join(root, "sindri-scan-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, "sindri-scan-file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(t, file, 3*time.Hour)
		res, err := SweepStaging(root, time.Hour)
		if err != nil || !exists(target) || !exists(link) || !exists(file) || res.Removed != 0 {
			t.Fatalf("err=%v result=%+v", err, res)
		}
	})
	t.Run("MissingParentIsNotAnError", func(t *testing.T) {
		res, err := SweepStaging(filepath.Join(t.TempDir(), "absent"), time.Hour)
		if err != nil || res.Removed != 0 {
			t.Fatalf("err=%v result=%+v", err, res)
		}
	})
}
