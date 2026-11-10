package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/queue"
)

func TestScanMetrics_WrapCountsEachResult(t *testing.T) {
	tests := []struct {
		name   string
		result string
		err    error
		want   string
	}{
		{name: "AttachedEvidenceIsSucceeded", result: `{"sbom_id":"a"}`, want: "succeeded"},
		{name: "SkippedResultIsSkippedNotSucceeded", result: `{"skipped":"unsupported kind"}`, want: "skipped"},
		{name: "ErrorIsFailed", err: errors.New("trivy failed"), want: "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := newScanMetrics(reg)
			if err != nil {
				t.Fatal(err)
			}
			h := m.wrap(func(context.Context, queue.Job) (json.RawMessage, error) {
				return json.RawMessage(tc.result), tc.err
			})
			if _, err := h(t.Context(), queue.Job{}); !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the handler's %v passed through", err, tc.err)
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, mf := range families {
				switch mf.GetName() {
				case "sindri_scans_total":
					for _, metric := range mf.GetMetric() {
						want := 0.0
						if metric.GetLabel()[0].GetValue() == tc.want {
							want = 1
						}
						if got := metric.GetCounter().GetValue(); got != want {
							t.Errorf("%s = %v, want %v", metric.GetLabel()[0].GetValue(), got, want)
						}
					}
				case "sindri_scan_duration_seconds":
					if got := mf.GetMetric()[0].GetHistogram().GetSampleCount(); got != 1 {
						t.Errorf("duration observations = %d, want 1", got)
					}
				}
			}
		})
	}
}

func TestBuildScanner_UnsafeCacheDirNamesTheSetting(t *testing.T) {
	dir := t.TempDir() + "/loose"
	if err := mkdirMode(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, _, err := buildScanner(t.Context(), scanningWithCache(dir), nil, nil, nil, prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "scanning.cacheDir") {
		t.Fatalf("err = %v, want a refusal naming scanning.cacheDir", err)
	}
}

func scanningWithCache(dir string) config.Scanning {
	s := config.Default().Scanning
	s.Enabled, s.CacheDir = true, dir
	return s
}

func mkdirMode(dir string, mode os.FileMode) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, mode) //nolint:gosec // the test needs a loose directory to prove it is refused
}
