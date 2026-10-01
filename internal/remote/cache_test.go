package remote_test

import (
	"testing"
	"time"

	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

func TestEvaluateCache(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour
	neg := 5 * time.Minute

	cases := []struct {
		name     string
		entry    registry.CacheEntry
		err      error
		ttl, neg time.Duration
		want     remote.CacheDecision
	}{
		{
			name: "NoEntry",
			err:  registry.ErrNotFound,
			ttl:  ttl,
			neg:  neg,
			want: remote.CacheMiss,
		},
		{
			name:  "FreshHit",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheHit, FetchedAt: now.Add(-time.Minute)},
			ttl:   ttl,
			neg:   neg,
			want:  remote.CacheFreshHit,
		},
		{
			name:  "StaleHit",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheHit, FetchedAt: now.Add(-2 * time.Hour)},
			ttl:   ttl,
			neg:   neg,
			want:  remote.CacheStaleHit,
		},
		{
			name:  "FreshNegative",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheMiss, FetchedAt: now.Add(-time.Minute)},
			ttl:   ttl,
			neg:   neg,
			want:  remote.CacheFreshNegative,
		},
		{
			name:  "ExpiredNegative",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheMiss, FetchedAt: now.Add(-time.Hour)},
			ttl:   ttl,
			neg:   neg,
			want:  remote.CacheMiss,
		},
		{
			name:  "ZeroTTLNeverExpiresHit",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheHit, FetchedAt: now.Add(-24 * time.Hour)},
			ttl:   0,
			neg:   neg,
			want:  remote.CacheFreshHit,
		},
		{
			name:  "ZeroNegativeDoesNotCacheMiss",
			entry: registry.CacheEntry{Key: "k", Status: registry.CacheMiss, FetchedAt: now},
			ttl:   ttl,
			neg:   0,
			want:  remote.CacheMiss,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remote.EvaluateCache(tc.entry, now, tc.ttl, tc.neg, tc.err)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
