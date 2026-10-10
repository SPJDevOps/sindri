package main

import (
	"slices"
	"testing"
	"time"

	"github.com/spj/sindri/internal/config"
)

func TestRestartSections(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Config)
		want   []string
	}{
		{name: "NothingChanged", change: func(*config.Config) {}},
		{
			name:   "Worker",
			change: func(c *config.Config) { c.Worker.GC.Interval = config.Duration(time.Hour) },
			want:   []string{"worker"},
		},
		{
			name:   "AdminsReloadInPlace",
			change: func(c *config.Config) { c.Auth.Admins = []string{"ops"} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := config.Default()
			after := config.Default()
			tc.change(&after)
			if got := restartSections(&before, &after); !slices.Equal(got, tc.want) {
				t.Fatalf("restartSections = %v, want %v", got, tc.want)
			}
		})
	}
}
