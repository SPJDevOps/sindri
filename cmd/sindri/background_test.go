package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/notify"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
	"github.com/spj/sindri/internal/worker"
)

func TestSchedules_RescanOnlyWhenScanningIsOnAndTheIntervalIsPositive(t *testing.T) {
	tests := []struct {
		name     string
		scanning config.Scanning
		want     time.Duration
	}{
		{name: "ScanningOffHasNoRescan", scanning: config.Scanning{Enabled: false, RescanInterval: config.Duration(time.Hour)}},
		{name: "ZeroIntervalHasNoRescan", scanning: config.Scanning{Enabled: true}},
		{name: "ScanningOnSchedulesRescan", scanning: config.Scanning{Enabled: true, RescanInterval: config.Duration(6 * time.Hour)}, want: 6 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got time.Duration
			for _, s := range schedules(config.Worker{}, tt.scanning) {
				if s.Kind == scanjob.RescanJobKind {
					got = s.Interval
				}
			}
			if got != tt.want {
				t.Errorf("rescan interval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkerLanes_ScansAndDeliveriesRunInTheirOwnLoops(t *testing.T) {
	noop := func(context.Context, queue.Job) (json.RawMessage, error) { return nil, nil }
	handlers := map[string]worker.Handler{
		gc.JobKind: noop, jobEvictProxyCache: noop, jobPurgeUploads: noop,
		scanjob.JobKind: noop, scanjob.RescanJobKind: noop, notify.JobKind: noop,
	}
	scheds := []worker.Schedule{
		{Kind: jobEvictProxyCache, Interval: time.Hour},
		{Kind: gc.JobKind, Interval: time.Hour},
		{Kind: scanjob.RescanJobKind, Interval: time.Hour},
	}
	base := worker.Options{Owner: "host/1/ab", Handlers: handlers, Schedules: scheds}

	lanes := workerLanes(base)
	if len(lanes) != 3 {
		t.Fatalf("lanes = %d, want 3", len(lanes))
	}
	owners := map[string]bool{}
	seenHandlers := map[string]int{}
	seenScheds := map[string]int{}
	for _, l := range lanes {
		owners[l.Owner] = true
		for k := range l.Handlers {
			seenHandlers[k]++
		}
		for _, s := range l.Schedules {
			seenScheds[s.Kind]++
			if _, ok := l.Handlers[s.Kind]; !ok {
				t.Errorf("lane %s schedules %s without its handler", l.Owner, s.Kind)
			}
		}
	}
	if len(owners) != 3 {
		t.Errorf("owners = %v, want three distinct", owners)
	}
	for k := range handlers {
		if seenHandlers[k] != 1 {
			t.Errorf("handler %s in %d lanes, want exactly 1", k, seenHandlers[k])
		}
	}
	for _, s := range scheds {
		if seenScheds[s.Kind] != 1 {
			t.Errorf("schedule %s in %d lanes, want exactly 1", s.Kind, seenScheds[s.Kind])
		}
	}
	for _, l := range lanes {
		_, scans := l.Handlers[scanjob.JobKind]
		_, gcs := l.Handlers[gc.JobKind]
		_, hooks := l.Handlers[notify.JobKind]
		if (scans && (gcs || hooks)) || (hooks && gcs) {
			t.Errorf("lane %s mixes scan, delivery, and housekeeping kinds", l.Owner)
		}
	}
}

func TestWorkerLanes_AbsentKindsMakeNoEmptyLane(t *testing.T) {
	noop := func(context.Context, queue.Job) (json.RawMessage, error) { return nil, nil }
	tests := []struct {
		name       string
		handlers   map[string]worker.Handler
		wantLanes  int
		wantOwners []string
	}{
		{name: "WithoutScanningDeliveriesStillHaveALane", handlers: map[string]worker.Handler{gc.JobKind: noop, notify.JobKind: noop}, wantLanes: 2, wantOwners: []string{"o", "o-notify"}},
		{name: "OnlyHousekeepingIsOneLane", handlers: map[string]worker.Handler{gc.JobKind: noop}, wantLanes: 1, wantOwners: []string{"o"}},
		{name: "OnlyDeliveriesKeepTheOriginalOwner", handlers: map[string]worker.Handler{notify.JobKind: noop}, wantLanes: 1, wantOwners: []string{"o"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workerLanes(worker.Options{Owner: "o", Handlers: tt.handlers})
			var owners []string
			for _, l := range got {
				owners = append(owners, l.Owner)
			}
			if len(got) != tt.wantLanes || fmt.Sprint(owners) != fmt.Sprint(tt.wantOwners) {
				t.Errorf("lanes owned by %v, want %v", owners, tt.wantOwners)
			}
		})
	}
}
