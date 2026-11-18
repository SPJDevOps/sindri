package model

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// Version ordering for the two package protocols that have one. Both
// comparators are total: a string that is not a valid version sorts below every
// valid one, and two invalid strings compare as plain strings, so a list sorted
// with them has one stable order whatever it holds.

// CompareSemver compares two npm versions by semver precedence: -1, 0, or 1.
// Build metadata is ignored, as semver says; equal precedence falls back to the
// strings so the order is stable.
func CompareSemver(a, b string) int {
	va, vb := semver.IsValid("v"+a), semver.IsValid("v"+b)
	switch {
	case va && vb:
		if c := semver.Compare("v"+a, "v"+b); c != 0 {
			return c
		}
	case va:
		return 1
	case vb:
		return -1
	}
	return strings.Compare(a, b)
}

// pep440 matches a version in the normalised and the permitted loose spellings
// of PEP 440's appendix B, lowercased: an optional epoch, the release, then
// pre-release, post-release, dev, and local segments.
var pep440 = regexp.MustCompile(`^v?(?:(\d+)!)?(\d+(?:\.\d+)*)` +
	`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?(\d+)?)?` +
	`(?:(?:-(\d+))|(?:[-_.]?(post|rev|r)[-_.]?(\d+)?))?` +
	`(?:[-_.]?(dev)[-_.]?(\d+)?)?` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`)

// pep440Key is a parsed version in the form PEP 440 orders by.
type pep440Key struct {
	epoch   int
	release []int
	// pre ranks the pre-release segment: a dev-only release sorts below every
	// pre-release (-1), a pre-release by its letter and number, and a version
	// with none above all of them.
	preRank, preNum int
	post            int // -1 when there is no post-release
	dev             int // -1 when there is none, which sorts above any dev
	hasDev          bool
	local           []string
}

const (
	preDevOnly = -1
	preAlpha   = 0
	preBeta    = 1
	preRC      = 2
	preNone    = 3
)

func parsePEP440(v string) (pep440Key, bool) {
	m := pep440.FindStringSubmatch(strings.ToLower(strings.TrimSpace(v)))
	if m == nil {
		return pep440Key{}, false
	}
	var k pep440Key
	num := func(s string) (int, bool) {
		if s == "" {
			return 0, true
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	var ok bool
	if k.epoch, ok = num(m[1]); !ok {
		return pep440Key{}, false
	}
	for _, part := range strings.Split(m[2], ".") {
		n, ok := num(part)
		if !ok {
			return pep440Key{}, false
		}
		k.release = append(k.release, n)
	}
	// 1.0 and 1.0.0 are the same release.
	for len(k.release) > 1 && k.release[len(k.release)-1] == 0 {
		k.release = k.release[:len(k.release)-1]
	}

	k.post = -1
	k.preRank = preNone
	if m[3] != "" {
		switch m[3] {
		case "alpha", "a":
			k.preRank = preAlpha
		case "beta", "b":
			k.preRank = preBeta
		default: // c, rc, pre, preview
			k.preRank = preRC
		}
		if k.preNum, ok = num(m[4]); !ok {
			return pep440Key{}, false
		}
	}
	switch {
	case m[5] != "":
		if k.post, ok = num(m[5]); !ok {
			return pep440Key{}, false
		}
	case m[6] != "":
		if k.post, ok = num(m[7]); !ok {
			return pep440Key{}, false
		}
	}
	if m[8] != "" {
		k.hasDev = true
		if k.dev, ok = num(m[9]); !ok {
			return pep440Key{}, false
		}
	}
	if k.preRank == preNone && k.post < 0 && k.hasDev {
		k.preRank = preDevOnly
	}
	if m[10] != "" {
		k.local = strings.FieldsFunc(m[10], func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	}
	return k, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (a pep440Key) compare(b pep440Key) int {
	if c := cmpInt(a.epoch, b.epoch); c != 0 {
		return c
	}
	for i := range max(len(a.release), len(b.release)) {
		var x, y int
		if i < len(a.release) {
			x = a.release[i]
		}
		if i < len(b.release) {
			y = b.release[i]
		}
		if c := cmpInt(x, y); c != 0 {
			return c
		}
	}
	if c := cmpInt(a.preRank, b.preRank); c != 0 {
		return c
	}
	if c := cmpInt(a.preNum, b.preNum); c != 0 {
		return c
	}
	if c := cmpInt(a.post, b.post); c != 0 {
		return c
	}
	// A dev release sorts below the same version without one.
	switch {
	case a.hasDev && !b.hasDev:
		return -1
	case !a.hasDev && b.hasDev:
		return 1
	}
	if c := cmpInt(a.dev, b.dev); c != 0 {
		return c
	}
	return compareLocal(a.local, b.local)
}

// compareLocal orders local version labels: none is lowest, a numeric segment
// beats an alphabetic one, and a longer label beats its own prefix.
func compareLocal(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return cmpInt(len(a), len(b))
	}
	for i := range min(len(a), len(b)) {
		x, xerr := strconv.Atoi(a[i])
		y, yerr := strconv.Atoi(b[i])
		switch {
		case xerr == nil && yerr == nil:
			if c := cmpInt(x, y); c != 0 {
				return c
			}
		case xerr == nil:
			return 1
		case yerr == nil:
			return -1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

// ComparePEP440 compares two PyPI versions by PEP 440 precedence: -1, 0, or 1.
func ComparePEP440(a, b string) int {
	ka, oka := parsePEP440(a)
	kb, okb := parsePEP440(b)
	switch {
	case oka && okb:
		return ka.compare(kb)
	case oka:
		return 1
	case okb:
		return -1
	}
	return strings.Compare(a, b)
}

// VersionComparator returns the ordering a protocol's versions follow, or nil
// for a protocol whose versions have none (OCI tags are names, not versions).
func VersionComparator(p Protocol) func(a, b string) int {
	switch p {
	case ProtocolNPM:
		return CompareSemver
	case ProtocolPyPI:
		return ComparePEP440
	case ProtocolOCI:
	}
	return nil
}

// HighestVersion returns the highest of versions under the protocol's
// ordering, or "" for an empty list or a protocol without one.
func HighestVersion(p Protocol, versions []string) string {
	cmp := VersionComparator(p)
	if cmp == nil || len(versions) == 0 {
		return ""
	}
	return slices.MaxFunc(versions, cmp)
}
