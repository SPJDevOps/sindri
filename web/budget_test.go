package web_test

import (
	"io/fs"
	"path"
	"testing"

	"github.com/spj/sindri/web"
)

// maxOwnScript is the budget for each script Sindri writes itself. The UI's
// behaviour is htmx plus a little of ours; a script that outgrows this is a
// framework arriving by stages, which the single-binary, no-build-step promise
// rules out. The vendored htmx is not ours and is pinned by VENDOR.md instead.
const maxOwnScript = 5 << 10

const vendoredScript = "htmx.min.js"

func TestOwnScripts_StayUnderBudget(t *testing.T) {
	checked := 0
	err := fs.WalkDir(web.Static(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".js" || path.Base(p) == vendoredScript {
			return nil
		}
		checked++
		b, err := fs.ReadFile(web.Static(), p)
		if err != nil {
			return err
		}
		if len(b) > maxOwnScript {
			t.Errorf("%s is %d bytes, over the %d byte budget", p, len(b), maxOwnScript)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets: %v", err)
	}
	// sindri.js exists, so finding nothing means the walk is wrong, not that
	// there is nothing to measure.
	if checked == 0 {
		t.Fatal("found no scripts of our own under web/static; sindri.js at least should be measured")
	}
}
