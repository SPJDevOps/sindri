package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// SIN-116: net/http masks a URL's password in a *url.Error but prints its user
// name, which is where a token is often written. A stored upstream URL that
// predates the refusal must not leak that into an error or a log line, on any
// path through down(), and the wrapping around it must survive.
func TestDown_DoesNotCarryUpstreamUserinfo(t *testing.T) {
	sentinel := errors.New("sentinel")
	tests := []struct {
		name      string
		cause     error
		wantIs    error
		wantDown  bool
		wantInTxt string
	}{
		{"Outage", errors.New("connection refused"), nil, true, "connection refused"},
		{"Canceled", context.Canceled, context.Canceled, false, "canceled"},
		{"RedirectRefused", errRedirectRefused, errRedirectRefused, false, "redirect"},
		{"WrappedSentinel", fmt.Errorf("%w: outer text", sentinel), sentinel, true, "outer text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue := &url.Error{Op: "Get", URL: "https://ghp_tok3n:***@registry.example.com/v2/x", Err: tc.cause}
			err := down(fmt.Errorf("wrapper text: %w", ue))
			if strings.Contains(err.Error(), "ghp_tok3n") {
				t.Errorf("error carries the user name: %v", err)
			}
			for _, want := range []string{"registry.example.com", "wrapper text", tc.wantInTxt} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lost %q: %v", want, err)
				}
			}
			if got := errors.Is(err, errUpstreamDown); got != tc.wantDown {
				t.Errorf("errors.Is(errUpstreamDown) = %v, want %v", got, tc.wantDown)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("sentinel %v does not survive: %v", tc.wantIs, err)
			}
		})
	}
}

func TestResolveFileURL_ErrorsDoNotEchoTheURL(t *testing.T) {
	for _, tc := range []struct{ name, base, file string }{
		{"BadBase", "http://alice:s3cret@exa mple.com/%zz", "x.whl"},
		{"BadFile", "https://registry.example.com", "http://alice:s3cret@host/%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveFileURL(tc.base, tc.file)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "alice") {
				t.Errorf("error echoes the URL: %v", err)
			}
		})
	}
}

// The scrub fails closed and hands errors.As only a scrubbed copy.
func TestScrubUserinfo_FailsClosedAndExposesOnlyAScrubbedCopy(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: "https://ghp_tok3n:***@registry.example.com/v2/x", Err: context.Canceled}

	t.Run("AsYieldsAScrubbedCopy", func(t *testing.T) {
		err := scrubUserinfo(fmt.Errorf("wrapper: %w", ue))
		var got *url.Error
		if !errors.As(err, &got) {
			t.Fatal("errors.As finds no *url.Error")
		}
		if strings.Contains(got.URL, "ghp_tok3n") || strings.Contains(got.Error(), "ghp_tok3n") {
			t.Errorf("the *url.Error carries the user name: %v", got)
		}
		if !errors.Is(err, context.Canceled) {
			t.Error("errors.Is(context.Canceled) does not hold")
		}
	})

	t.Run("FallsBackWhenTheTextStillShowsTheUserName", func(t *testing.T) {
		err := scrubUserinfo(oddError{ue})
		if strings.Contains(err.Error(), "ghp_tok3n") {
			t.Errorf("text leaks: %v", err)
		}
		var got *url.Error
		if !errors.As(err, &got) || strings.Contains(got.Error(), "ghp_tok3n") {
			t.Errorf("As leaks or fails: %v", got)
		}
	})
}

// oddError prints the URL in a form the scrub's replacement cannot match.
type oddError struct{ ue *url.Error }

func (e oddError) Error() string { return "failed for user ghp_tok3n" }
func (e oddError) Unwrap() error { return e.ue }

func TestNPMTarballURL_ErrorsDoNotEchoTheURL(t *testing.T) {
	_, err := npmTarballURL("http://alice:s3cret@exa mple.com/%zz", "pkg/-/pkg-1.0.0.tgz")
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "alice") {
		t.Errorf("error echoes the URL: %v", err)
	}
}
