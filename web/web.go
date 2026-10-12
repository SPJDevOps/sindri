// Package web holds the read-only UI's templates and static assets, embedded
// so that the UI ships inside the one binary with no Node build and no CDN
// (ARCHITECTURE.md section 3, ADR 21). The handlers live in internal/ui;
// this package is only the files, because go:embed cannot reach a parent
// directory.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

//go:embed templates static VENDOR.md
var files embed.FS

// Templates is the html/template sources.
func Templates() fs.FS { return sub("templates") }

// Static is the CSS, scripts, and fonts the pages load.
func Static() fs.FS { return sub("static") }

// Vendor is VENDOR.md, which records the checksum of every vendored file.
func Vendor() ([]byte, error) { return files.ReadFile("VENDOR.md") }

// Version is a hash over every static file. Assets are served under it, so a
// browser may cache them forever and still fetches new ones after an upgrade.
var Version = func() string {
	h := sha256.New()
	var paths []string
	_ = fs.WalkDir(files, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	for _, p := range paths {
		b, _ := files.ReadFile(p)
		h.Write([]byte(p))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

func sub(dir string) fs.FS {
	f, err := fs.Sub(files, dir)
	if err != nil {
		panic("web: " + err.Error()) // impossible: dir is embedded above
	}
	return f
}
