package main

import (
	"testing"
	"time"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/scanjob"
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
