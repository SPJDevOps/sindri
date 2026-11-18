package model

import (
	"slices"
	"testing"
)

func TestCompareSemver_OrdersByPrecedence(t *testing.T) {
	// Each list is in ascending order.
	tests := []struct {
		name string
		asc  []string
	}{
		{"numeric parts compare as numbers", []string{"1.2.9", "1.2.10", "1.10.0", "2.0.0"}},
		{"a prerelease sorts below its release", []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-beta", "1.0.0-rc.1", "1.0.0"}},
		{"an invalid version sorts below every valid one", []string{"latest", "0.0.1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.asc {
				for j := range tc.asc {
					want := cmpInt(i, j)
					if got := CompareSemver(tc.asc[i], tc.asc[j]); got != want {
						t.Errorf("CompareSemver(%q, %q) = %d, want %d", tc.asc[i], tc.asc[j], got, want)
					}
				}
			}
		})
	}
}

func TestComparePEP440_OrdersByPrecedence(t *testing.T) {
	tests := []struct {
		name string
		asc  []string
	}{
		{"numeric parts compare as numbers", []string{"1.2.9", "1.2.10", "1.10", "2.0"}},
		{"the full pre, release, post order", []string{
			"1.0.dev1", "1.0a1", "1.0a2", "1.0b1", "1.0rc1", "1.0", "1.0.post1", "1.1.dev1", "1.1"}},
		{"a dev release sorts below the version it precedes", []string{"1.0a1.dev1", "1.0a1", "1.0.post1.dev1", "1.0.post1"}},
		{"an epoch beats the release", []string{"2.0", "1!0.1"}},
		{"a local label sorts above the plain version", []string{"1.0", "1.0+abc", "1.0+abd", "1.0+5"}},
		{"an invalid version sorts below every valid one", []string{"not-a-version", "0.0.1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.asc {
				for j := range tc.asc {
					want := cmpInt(i, j)
					if got := ComparePEP440(tc.asc[i], tc.asc[j]); got != want {
						t.Errorf("ComparePEP440(%q, %q) = %d, want %d", tc.asc[i], tc.asc[j], got, want)
					}
				}
			}
		})
	}
}

func TestComparePEP440_SpellingsOfOneVersionAreEqual(t *testing.T) {
	for _, pair := range [][2]string{{"1.0", "1.0.0"}, {"1.0-1", "1.0.post1"}, {"1.0ALPHA1", "1.0a1"}, {"1.0.rc1", "1.0c1"}, {"v1.0", "1.0"}} {
		if got := ComparePEP440(pair[0], pair[1]); got != 0 {
			t.Errorf("ComparePEP440(%q, %q) = %d, want 0", pair[0], pair[1], got)
		}
	}
}

func TestHighestVersion_PicksByTheProtocolsOrdering(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		in       []string
		want     string
	}{
		{"npm semver, not lexical", ProtocolNPM, []string{"1.9.0", "1.10.0", "1.2.0"}, "1.10.0"},
		{"npm release above its prerelease", ProtocolNPM, []string{"2.0.0-rc.1", "2.0.0", "1.9.0"}, "2.0.0"},
		{"pypi pep 440", ProtocolPyPI, []string{"1.0", "1.0.post1", "1.0rc1"}, "1.0.post1"},
		{"oci has no ordering", ProtocolOCI, []string{"v1", "v2"}, ""},
		{"empty", ProtocolNPM, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HighestVersion(tc.protocol, slices.Clone(tc.in)); got != tc.want {
				t.Errorf("HighestVersion = %q, want %q", got, tc.want)
			}
		})
	}
}
