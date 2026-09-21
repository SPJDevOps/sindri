package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestStringContainsBuildMetadata(t *testing.T) {
	got := String()
	for _, want := range []string{"sindri", Version, Commit, runtime.GOOS} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
