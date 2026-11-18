package ui

import (
	"encoding/json"
	"net/url"
	"slices"
	"testing"

	"github.com/spj/sindri/internal/lifecycle"
)

func scans(raw ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(raw))
	for i, r := range raw {
		out[i] = json.RawMessage(r)
	}
	return out
}

func TestSeverityOf_SumsScansAndNeverLooksCleanWithoutOne(t *testing.T) {
	high := `{"producer":"sindri","severity_counts":{"HIGH":2,"LOW":1}}`
	arm := `{"producer":"sindri","platform":"linux/arm64","severity_counts":{"HIGH":1,"CRITICAL":1}}`
	clean := `{"producer":"sindri","severity_counts":{"HIGH":0}}`
	amd := `{"producer":"sindri","platform":"linux/amd64","platforms":["linux/amd64","linux/arm64"],"created_at":"2026-01-01T00:00:00Z","severity_counts":{"HIGH":0}}`
	armListed := `{"producer":"sindri","platform":"linux/arm64","platforms":["linux/amd64","linux/arm64"],"created_at":"2026-01-01T00:00:00Z","severity_counts":{"HIGH":1}}`
	client := `{"producer":"ci","severity_counts":{"CRITICAL":9}}`
	tests := []struct {
		name        string
		sums        []lifecycle.SubjectSummary
		wantScanned bool
		want        []severityCount
	}{
		{"NothingToSummarise", nil, false, nil},
		{"NoScanIsNotScanned", []lifecycle.SubjectSummary{{}}, false, nil},
		{"ACleanScanIsScanned", []lifecycle.SubjectSummary{{Scans: scans(clean)}}, true, nil},
		{"PlatformsAddUp", []lifecycle.SubjectSummary{{Scans: scans(high, arm)}}, true,
			[]severityCount{{"critical", "sev-critical", 1}, {"high", "sev-high", 3}, {"low", "sev-low", 1}}},
		{"AnIndexWithOnlyOneListedPlatformScannedIsNotScanned", []lifecycle.SubjectSummary{{Scans: scans(amd)}}, false, nil},
		{"AnIndexWithEveryListedPlatformScannedIsScanned", []lifecycle.SubjectSummary{{Scans: scans(amd, armListed)}}, true,
			[]severityCount{{"high", "sev-high", 1}}},
		{"AClientScanDoesNotCount", []lifecycle.SubjectSummary{{Scans: scans(client)}}, false, nil},
		{"AnUnscannedFileMakesTheVersionNotScanned", []lifecycle.SubjectSummary{{Scans: scans(clean)}, {}}, false, nil},
		{"FindingsStillShowWhenAnotherFileIsUnscanned", []lifecycle.SubjectSummary{{Scans: scans(high)}, {}}, false,
			[]severityCount{{"high", "sev-high", 2}, {"low", "sev-low", 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := severityOf(tc.sums)
			if got.Scanned != tc.wantScanned || !slices.Equal(got.Counts, tc.want) {
				t.Errorf("severityOf = %+v, want scanned=%v counts=%v", got, tc.wantScanned, tc.want)
			}
		})
	}
}

func TestWorstOf_KeepsOnlyTheHighestSeverity(t *testing.T) {
	v := severityView{Scanned: true, Counts: []severityCount{{"high", "sev-high", 3}, {"low", "sev-low", 1}}}
	if got := worstOf(v); len(got.Counts) != 1 || got.Counts[0].Severity != "high" || got.Counts[0].N != 3 {
		t.Errorf("worstOf = %+v", got)
	}
	if got := worstOf(severityView{Scanned: true}); !got.Scanned || len(got.Counts) != 0 {
		t.Errorf("worstOf of a clean scan = %+v", got)
	}
}

func TestStageOf_IsTheHighestApprovedStageOfTheLeastPromotedFile(t *testing.T) {
	appr := func(repo, stage string, pos int32) lifecycle.StageApproval {
		return lifecycle.StageApproval{Repository: repo, Stage: stage, Position: pos, Action: lifecycle.ActionApprove}
	}
	rev := func(repo, stage string, pos int32) lifecycle.StageApproval {
		return lifecycle.StageApproval{Repository: repo, Stage: stage, Position: pos, Action: lifecycle.ActionRevoke}
	}
	tests := []struct {
		name string
		sums []lifecycle.SubjectSummary
		want stageView
	}{
		{"None", nil, stageView{}},
		{"NotApproved", []lifecycle.SubjectSummary{{}}, stageView{}},
		{"HighestApprovedStage", []lifecycle.SubjectSummary{{Approvals: []lifecycle.StageApproval{appr("r", "prod", 2)}}}, stageView{Name: "prod"}},
		{"AnotherRepositoryDoesNotCount", []lifecycle.SubjectSummary{{Approvals: []lifecycle.StageApproval{appr("other", "prod", 2)}}}, stageView{}},
		{"TwoFilesGoOnlyAsFarAsTheLeastPromoted", []lifecycle.SubjectSummary{
			{Approvals: []lifecycle.StageApproval{appr("r", "prod", 2)}},
			{Approvals: []lifecycle.StageApproval{appr("r", "dev", 0)}}}, stageView{Name: "dev"}},
		{"OneUnpromotedFileMeansNone", []lifecycle.SubjectSummary{
			{Approvals: []lifecycle.StageApproval{appr("r", "prod", 2)}}, {}}, stageView{}},
		{"ARevokedTopStageReadsRevokedFromIt", []lifecycle.SubjectSummary{{Approvals: []lifecycle.StageApproval{rev("r", "dev", 0)}}},
			stageView{Name: "dev", Revoked: true}},
		{"ARevokeOfTheTopStageIsNotHiddenByALowerApproval", []lifecycle.SubjectSummary{
			{Approvals: []lifecycle.StageApproval{appr("r", "dev", 0), rev("r", "prod", 2)}}}, stageView{Name: "prod", Revoked: true}},
		{"ARevokeOfALowerStageDoesNotHideAHigherApproval", []lifecycle.SubjectSummary{
			{Approvals: []lifecycle.StageApproval{rev("r", "dev", 0), appr("r", "prod", 2)}}}, stageView{Name: "prod"}},
		{"ARevokedFileMakesTheVersionRevoked", []lifecycle.SubjectSummary{
			{Approvals: []lifecycle.StageApproval{appr("r", "prod", 2)}},
			{Approvals: []lifecycle.StageApproval{rev("r", "dev", 0)}}}, stageView{Name: "dev", Revoked: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageOf(tc.sums, "r"); got != tc.want {
				t.Errorf("stageOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOwnersOf_ListsOnlyOwnersTheCallerMayRead(t *testing.T) {
	sums := []lifecycle.SubjectSummary{
		{Owners: []string{"repository:team-a", "repository:team-b"}},
		{Owners: []string{"repository:team-a", "user:someone"}},
	}
	got := ownersOf(sums, map[string]bool{"team-a": true})
	if want := []string{"repository:team-a"}; !slices.Equal(got, want) {
		t.Errorf("ownersOf = %v, want %v", got, want)
	}
}

func TestPkgFilter_ParsesAndRendersTheURL(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  pkgFilter
		href  string
	}{
		{"Defaults", "", pkgFilter{Sort: sortName}, "/ui/r/team/app"},
		{"QueryAndSort", "q=web&sort=published&page=3", pkgFilter{Q: "web", Sort: sortPublished, Page: 3}, "/ui/r/team/app?page=3&q=web&sort=published"},
		{"UnknownSortAndBadPageAreDropped", "sort=x&page=-4", pkgFilter{Sort: sortName}, "/ui/r/team/app"},
		{"AHugePageIsCapped", "page=99999999999999", pkgFilter{Sort: sortName, Page: maxPage}, "/ui/r/team/app?page=100000"},
		{"QueryIsTrimmed", "q=%20web%20", pkgFilter{Q: "web", Sort: sortName}, "/ui/r/team/app?q=web"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := url.ParseQuery(tc.query)
			got := parsePkgFilter(v)
			if got != tc.want {
				t.Errorf("parsePkgFilter = %+v, want %+v", got, tc.want)
			}
			if href := got.href("team/app"); href != tc.href {
				t.Errorf("href = %q, want %q", href, tc.href)
			}
		})
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := parsePkgFilter(url.Values{"q": {string(long)}}); len([]rune(got.Q)) != maxFilterLen {
		t.Errorf("a %d-character query was not cut to %d", len(long), maxFilterLen)
	}
}
