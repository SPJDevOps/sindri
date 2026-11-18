package ui

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func sindriScan(id string, at time.Time, platform string, counts string) evidence.Evidence {
	return evidence.Evidence{
		ID: id, Type: model.EvidenceScan, CreatedAt: at,
		Metadata: json.RawMessage(`{"producer":"sindri","platform":"` + platform + `","severity_counts":` + counts + `}`),
	}
}

func TestVerdict_ReadsStageScanAndNewFindings(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	position := map[string]int32{"dev": 0, "staging": 1, "prod": 2}
	state := func(repo, stage string, action lifecycle.Action, at time.Time) lifecycle.State {
		return lifecycle.State{Repository: repo, Stage: stage, Action: action, Actor: "user:alice", At: at}
	}
	none := `{"CRITICAL":0,"HIGH":0,"MEDIUM":0,"LOW":0,"UNKNOWN":0}`
	high1 := `{"CRITICAL":0,"HIGH":1,"MEDIUM":0,"LOW":0,"UNKNOWN":0}`
	high2 := `{"CRITICAL":0,"HIGH":2,"MEDIUM":0,"LOW":0,"UNKNOWN":0}`
	medium := `{"CRITICAL":0,"HIGH":0,"MEDIUM":3,"LOW":0,"UNKNOWN":0}`
	low := `{"CRITICAL":0,"HIGH":0,"MEDIUM":0,"LOW":5,"UNKNOWN":0}`

	tests := []struct {
		name        string
		states      []lifecycle.State
		evs         []evidence.Evidence
		stage       string
		revoked     bool
		scanned     bool
		worst       string
		newFindings bool
	}{
		{name: "NotPromotedAndNotScanned"},
		{
			name:   "ApprovedAtTheHighestStage",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1)), state("r", "staging", lifecycle.ActionApprove, hour(2))},
			stage:  "staging",
		},
		{
			name:   "RevokedFromTheHighestStageReached",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1)), state("r", "prod", lifecycle.ActionRevoke, hour(3))},
			stage:  "prod", revoked: true,
		},
		{
			name:   "ARevokeOfALowerStageDoesNotHideAHigherApproval",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionRevoke, hour(3)), state("r", "prod", lifecycle.ActionApprove, hour(1))},
			stage:  "prod",
		},
		{
			name:   "OnlyThisRepositoryCounts",
			states: []lifecycle.State{state("other", "prod", lifecycle.ActionApprove, hour(1)), state("r", "dev", lifecycle.ActionApprove, hour(1))},
			stage:  "dev",
		},
		{
			name:   "AnUnknownStageIsIgnored",
			states: []lifecycle.State{state("r", "gone", lifecycle.ActionApprove, hour(1))},
		},
		{
			name:   "ScannedCleanIsNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1))},
			evs:    []evidence.Evidence{sindriScan("a", hour(2), "", none)},
			stage:  "dev", scanned: true,
		},
		{
			name:   "WorseScanAfterApprovalIsNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", high1), sindriScan("b", hour(3), "", high2)},
			stage:  "dev", scanned: true, worst: "2 high", newFindings: true,
		},
		{
			name:   "AFirstScanWithHighFindingsAfterApprovalIsNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1))},
			evs:    []evidence.Evidence{sindriScan("a", hour(2), "", high1)},
			stage:  "dev", scanned: true, worst: "1 high", newFindings: true,
		},
		{
			name:   "GrowthBelowHighAloneIsNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", low), sindriScan("b", hour(3), "", medium)},
			stage:  "dev", scanned: true, worst: "3 medium",
		},
		{
			name:   "AFirstScanWithOnlyMediumAfterApprovalIsNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1))},
			evs:    []evidence.Evidence{sindriScan("a", hour(2), "", medium)},
			stage:  "dev", scanned: true, worst: "3 medium",
		},
		{
			name:   "HighGrowingFromOneToTwoIsNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", high1), sindriScan("b", hour(3), "", high2)},
			stage:  "dev", scanned: true, worst: "2 high", newFindings: true,
		},
		{
			name:   "TheSameFindingsAreNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", high1), sindriScan("b", hour(3), "", high1)},
			stage:  "dev", scanned: true, worst: "1 high",
		},
		{
			name:   "FewerFindingsAreNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", high2), sindriScan("b", hour(3), "", high1)},
			stage:  "dev", scanned: true, worst: "1 high",
		},
		{
			name:   "AScanBeforeTheApprovalIsNotNew",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(5))},
			evs:    []evidence.Evidence{sindriScan("a", hour(1), "", none), sindriScan("b", hour(3), "", high2)},
			stage:  "dev", scanned: true, worst: "2 high",
		},
		{
			name:   "NothingIsNewWithoutAnApproval",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionRevoke, hour(1))},
			evs:    []evidence.Evidence{sindriScan("a", hour(2), "", high2)},
			stage:  "dev", revoked: true, scanned: true, worst: "2 high",
		},
		{
			name:   "ANewerScanOfAnotherPlatformIsJudgedAgainstItsOwnHistory",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(2))},
			evs: []evidence.Evidence{
				sindriScan("a", hour(1), "linux/amd64", high2), sindriScan("b", hour(3), "linux/arm64", none),
			},
			stage: "dev", scanned: true, worst: "2 high",
		},
		{
			name:   "AClientAttachedScanNeverCounts",
			states: []lifecycle.State{state("r", "dev", lifecycle.ActionApprove, hour(1))},
			evs: []evidence.Evidence{{
				ID: "x", Type: model.EvidenceScan, CreatedAt: hour(2),
				Metadata: json.RawMessage(`{"producer":"someone","severity_counts":` + high2 + `}`),
			}},
			stage: "dev",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerdict(tc.states, "r", position, tc.evs)
			if v.Stage != tc.stage || v.Revoked != tc.revoked {
				t.Errorf("stage = %q revoked = %v, want %q, %v", v.Stage, v.Revoked, tc.stage, tc.revoked)
			}
			if v.Severity.Scanned != tc.scanned || v.ScannedAt.IsZero() == tc.scanned {
				t.Errorf("scanned = %v at %v, want scanned %v", v.Severity.Scanned, v.ScannedAt, tc.scanned)
			}
			got := ""
			if len(v.Severity.Counts) == 1 {
				got = itoa(v.Severity.Counts[0].N) + " " + v.Severity.Counts[0].Severity
			}
			if got != tc.worst {
				t.Errorf("worst severity = %q, want %q", got, tc.worst)
			}
			if v.NewFindings != tc.newFindings {
				t.Errorf("new findings = %v, want %v", v.NewFindings, tc.newFindings)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSizeBucket_RoundsUpToTenths(t *testing.T) {
	tests := []struct {
		name        string
		size, total int64
		want        int
	}{
		{"EmptyLayer", 0, 100, 0},
		{"NoTotal", 5, 0, 0},
		{"TinyLayerStillShows", 1, 1000, 1},
		{"Half", 50, 100, 5},
		{"JustOverATenth", 11, 100, 2},
		{"Whole", 100, 100, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sizeBucket(tc.size, tc.total); got != tc.want {
				t.Errorf("sizeBucket(%d, %d) = %d, want %d", tc.size, tc.total, got, tc.want)
			}
		})
	}
}

func TestBestName_PrefersTheTagTheURLNames(t *testing.T) {
	tags := []registry.DigestName{
		{IsTag: true, Package: "web", Ref: "1.0"},
		{IsTag: true, Package: "web", Ref: "latest"},
		{IsTag: true, Package: "api", Ref: "2.0"},
	}
	tests := []struct {
		name, image, tag, want string
	}{
		{"TheTagInTheURL", "web", "latest", "web:latest"},
		{"AStaleTagFallsBackToTheImage", "web", "gone", "web:1.0"},
		{"TheImageInTheURL", "api", "", "api:2.0"},
		{"NothingInTheURL", "", "", "web:1.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := bestName(model.Repository{Protocol: model.ProtocolOCI}, tags, tc.image, tc.tag)
			if !ok || n.Package+":"+n.Ref != tc.want {
				t.Errorf("bestName = %s:%s (%v), want %s", n.Package, n.Ref, ok, tc.want)
			}
		})
	}
	if _, ok := bestName(model.Repository{}, nil, "web", ""); ok {
		t.Error("bestName with no names reported one")
	}
}
