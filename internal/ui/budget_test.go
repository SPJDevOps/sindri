package ui

import (
	"os"
	"path/filepath"
	"testing"
)

// maxPageBytes is the budget for one rendered page's HTML. The golden files
// are the pages as rendered, so they are what is measured: a template change
// that doubles a page shows up here, before it shows up on a slow connection.
const maxPageBytes = 60 << 10

func TestGoldenPages_StayUnderBudget(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden pages found under testdata")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			info, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > maxPageBytes {
				t.Errorf("%s is %d bytes, over the %d byte budget", f, info.Size(), maxPageBytes)
			}
		})
	}
}
