package scan

import (
	"os"
	"regexp"
	"testing"
)

// The release image's Trivy and the one the integration tests run must be the
// same build, or the tests vouch for a scanner that is not shipped. The
// testsupport package is integration-tagged, so its pin is read as text.
func TestTrivyPin_ReleaseImageAndTestsupportAgree(t *testing.T) {
	ref := regexp.MustCompile(`aquasec/trivy:[0-9][^\s"]*@sha256:[0-9a-f]{64}`)
	find := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := ref.FindAllString(string(b), -1)
		if len(m) != 1 {
			t.Fatalf("%s holds %d pinned aquasec/trivy references, want exactly 1", path, len(m))
		}
		return m[0]
	}
	dockerfile := find("../../deploy/docker/Dockerfile.trivy")
	testsupport := find("../testsupport/trivy.go")
	if dockerfile != testsupport {
		t.Errorf("Dockerfile.trivy pins %s but internal/testsupport/trivy.go pins %s; move them together", dockerfile, testsupport)
	}
}
