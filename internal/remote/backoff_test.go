package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestNoteRefresh_OnlyAnOutageStartsABackoff(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, false},
		{"connection failure", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(errors.New("connection refused"))), true},
		{"timeout", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(context.DeadlineExceeded)), true},
		{"5xx is a fast answer", classify(http.StatusBadGateway), false},
		{"caller went away", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(context.Canceled)), false},
		{"redirect refused", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(errRedirectRefused)), false},
		{"body stalls after the headers", fmt.Errorf("%w: reading body: %w", ErrUpstreamUnavailable, context.DeadlineExceeded), false},
		{"401", classify(http.StatusUnauthorized), false},
		{"403", classify(http.StatusForbidden), false},
		{"429", classify(http.StatusTooManyRequests), false},
		{"unparsable document", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, errors.New("invalid character")), false},
		{"oversize document", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, errMetadataTooLarge), false},
		{"not found", classify(http.StatusNotFound), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{now: func() time.Time { return now }, backoffUntil: map[string]time.Time{}}
			c.noteRefresh("r", tc.err)
			if got := c.inBackoff("r"); got != tc.want {
				t.Fatalf("inBackoff after %v = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNoteRefresh_ASuccessEndsTheBackoffAndTheWindowExpires(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c := &Client{now: func() time.Time { return now }, backoffUntil: map[string]time.Time{}}
	outage := fmt.Errorf("%w: %w", ErrUpstreamUnavailable, down(errors.New("connection refused")))
	c.noteRefresh("r", outage)
	if !c.inBackoff("r") || c.inBackoff("other") {
		t.Fatal("a failure must back off its own repository only")
	}
	now = now.Add(refreshBackoff + time.Second)
	if c.inBackoff("r") {
		t.Fatal("the window did not expire")
	}
	c.noteRefresh("r", outage)
	c.noteRefresh("r", nil)
	if c.inBackoff("r") {
		t.Fatal("a success did not clear the backoff")
	}
}
