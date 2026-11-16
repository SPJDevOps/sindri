package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/gc"
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

func TestWorkerLanes_ScanKindsRunInTheirOwnLoop(t *testing.T) {
	noop := func(context.Context, queue.Job) (json.RawMessage, error) { return nil, nil }
	handlers := map[string]worker.Handler{
		gc.JobKind: noop, jobEvictProxyCache: noop, jobPurgeUploads: noop,
		scanjob.JobKind: noop, scanjob.RescanJobKind: noop,
	}
	scheds := []worker.Schedule{
		{Kind: jobEvictProxyCache, Interval: time.Hour},
		{Kind: gc.JobKind, Interval: time.Hour},
		{Kind: scanjob.RescanJobKind, Interval: time.Hour},
	}
	base := worker.Options{Owner: "host/1/ab", Handlers: handlers, Schedules: scheds}

	lanes := workerLanes(base)
	if len(lanes) != 2 {
		t.Fatalf("lanes = %d, want 2", len(lanes))
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
	if len(owners) != 2 {
		t.Errorf("owners = %v, want two distinct", owners)
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
		if scans == gcs {
			t.Errorf("lane %s mixes scan and non-scan kinds", l.Owner)
		}
	}

	delete(handlers, scanjob.JobKind)
	delete(handlers, scanjob.RescanJobKind)
	if got := workerLanes(worker.Options{Owner: "o", Handlers: handlers}); len(got) != 1 || got[0].Owner != "o" {
		t.Errorf("without scanning: %d lanes, want one with the original owner", len(got))
	}
}
