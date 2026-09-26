package pypi

import (
	"fmt"
	"strings"
)

// maxFilenameLength bounds a distribution filename. PyPI's own limit is
// smaller; this only has to stop a pathological name from reaching the
// database and the index HTML.
const maxFilenameLength = 256

// distFile is what a distribution filename claims about itself. PyPI treats
// the filename as data pip parses without asking the index, so it has to agree
// with the project and version the file is placed under.
type distFile struct {
	Project string // as spelled in the filename, not normalized
	Version string // as spelled in the filename, wheel-escaped
	Wheel   bool
}

// validateFilename rejects anything that is not a plausible distribution
// filename. This is the write-time validation the download path relies on:
// /pypi/{repo}/packages/{project}/{filename} has one path segment for the
// filename, so a name with a separator in it would be indexed and then be
// unreachable.
func validateFilename(filename string) error {
	switch {
	case filename == "":
		return fmt.Errorf("filename is required")
	case len(filename) > maxFilenameLength:
		return fmt.Errorf("filename is longer than %d bytes", maxFilenameLength)
	case strings.HasPrefix(filename, "."):
		return fmt.Errorf("filename may not start with a dot")
	case strings.Contains(filename, ".."):
		return fmt.Errorf("filename may not contain %q", "..")
	}
	for _, c := range filename {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+':
		default:
			return fmt.Errorf("filename contains an illegal character %q", c)
		}
	}
	if _, ok := trimDistSuffix(filename); !ok {
		return fmt.Errorf("filename must end in .whl, .tar.gz, or .tgz")
	}
	return nil
}

// trimDistSuffix removes a recognised distribution extension.
func trimDistSuffix(filename string) (base string, ok bool) {
	lower := strings.ToLower(filename)
	for _, suffix := range []string{".whl", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(lower, suffix) {
			return filename[:len(filename)-len(suffix)], true
		}
	}
	return "", false
}

// parseDistFilename reads the project and version out of a wheel or sdist
// filename.
//
// Wheels are unambiguous (PEP 427). Sdists are not: PEP 625 normalizes the
// name, but older files spell it freely, so the version is taken after the
// last hyphen. A PEP 440 version never contains a hyphen, which is what makes
// that split the conventional one.
func parseDistFilename(filename string) (distFile, error) {
	base, ok := trimDistSuffix(filename)
	if !ok {
		return distFile{}, fmt.Errorf("unrecognised distribution extension")
	}

	if strings.HasSuffix(strings.ToLower(filename), ".whl") {
		// {distribution}-{version}(-{build})?-{python}-{abi}-{platform}
		parts := strings.Split(base, "-")
		if len(parts) < 5 {
			return distFile{}, fmt.Errorf("wheel filename has %d hyphen-separated parts, want at least 5", len(parts))
		}
		if parts[0] == "" || parts[1] == "" {
			return distFile{}, fmt.Errorf("wheel filename has an empty name or version")
		}
		return distFile{Project: parts[0], Version: parts[1], Wheel: true}, nil
	}

	i := strings.LastIndex(base, "-")
	if i <= 0 || i == len(base)-1 {
		return distFile{}, fmt.Errorf("sdist filename must be {name}-{version}")
	}
	return distFile{Project: base[:i], Version: base[i+1:]}, nil
}

// escapeVersion folds a version the way a wheel filename spells it: runs of
// characters that are not alphanumeric or a dot become one underscore
// (PEP 427). Comparing escaped forms lets "1.0.0+local" in the metadata match
// "1.0.0_local" in the filename without accepting a different version.
func escapeVersion(version string) string {
	var b strings.Builder
	b.Grow(len(version))
	prevUnderscore := false
	for _, c := range version {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.':
			b.WriteRune(c)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return b.String()
}

// sameVersion reports whether two spellings of a version are the same version.
func sameVersion(a, b string) bool {
	return strings.EqualFold(escapeVersion(a), escapeVersion(b))
}
