package resolve

import "testing"

func TestSplitOCIMarkers_ReferrersIsOnlyTheLastTwoSegments(t *testing.T) {
	t.Parallel()
	const d = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name     string
		path     string
		wantName string
		wantRest string
		wantOK   bool
	}{
		{name: "referrers route", path: "team/app/referrers/" + d, wantName: "team/app", wantRest: "referrers/" + d, wantOK: true},
		{name: "single segment repository", path: "app/referrers/" + d, wantName: "app", wantRest: "referrers/" + d, wantOK: true},
		{name: "image named referrers keeps its manifests route", path: "team/referrers/app/manifests/1.0", wantName: "team/referrers/app", wantRest: "manifests/1.0", wantOK: true},
		{name: "image named referrers keeps its tags route", path: "team/referrers/tags/list", wantName: "team/referrers", wantRest: "tags/list", wantOK: true},
		{name: "referrers third from last is not a route", path: "team/referrers/app/extra", wantOK: false},
		{name: "bare referrers has no name", path: "referrers/" + d, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, rest, ok := splitOCIMarkers(tc.path)
			if ok != tc.wantOK || name != tc.wantName || rest != tc.wantRest {
				t.Fatalf("splitOCIMarkers(%q) = %q, %q, %v; want %q, %q, %v", tc.path, name, rest, ok, tc.wantName, tc.wantRest, tc.wantOK)
			}
		})
	}
}
