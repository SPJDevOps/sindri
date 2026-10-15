//go:build integration

package testsupport

import (
	"context"
	"errors"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// fakeContainer stands in for a container testcontainers created but could
// not start. Only Terminate is called on it; the embedded interface is nil.
type fakeContainer struct {
	testcontainers.Container
	terminated *int
}

func (f *fakeContainer) Terminate(context.Context, ...testcontainers.TerminateOption) error {
	*f.terminated++
	return nil
}

// errReaperRace is the error testcontainers returns when another test binary's
// reaper is found before it has started.
var errReaperRace = errors.New(`create container: reaper: from container "0123abcd": wait for reaper 0123abcd: unexpected container status "created"`)

// errPortNeverMapped is the error testcontainers returns when Docker never
// publishes a started container's port mapping.
var errPortNeverMapped = errors.New(`run postgres: generic container: start container: started hook: wait until ready: mapped port: check target: retries: 9, port: "invalid port", last err: get state: context deadline exceeded`)

func TestStartContainer(t *testing.T) {
	cases := []struct {
		name           string
		errs           []error // error per attempt; past the end, the attempt succeeds
		wantErr        bool
		wantAttempts   int
		wantTerminated int
	}{
		{
			name:           "RetriesReaperStartupRace",
			errs:           []error{errReaperRace},
			wantAttempts:   2,
			wantTerminated: 1,
		},
		{
			name:           "RetriesPortMappingThatNeverAppears",
			errs:           []error{errPortNeverMapped},
			wantAttempts:   2,
			wantTerminated: 1,
		},
		{
			name:           "GivesUpAfterThreeAttempts",
			errs:           []error{errReaperRace, errReaperRace, errReaperRace},
			wantErr:        true,
			wantAttempts:   3,
			wantTerminated: 3,
		},
		{
			name:           "DoesNotRetryOtherErrors",
			errs:           []error{errors.New("pull image: not found")},
			wantErr:        true,
			wantAttempts:   1,
			wantTerminated: 1,
		},
		{
			name:         "SucceedsFirstTime",
			wantAttempts: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts, terminated := 0, 0
			_, err := startContainer(t, t.Context(), func(context.Context) (*fakeContainer, error) {
				attempts++
				c := &fakeContainer{terminated: &terminated}
				if attempts <= len(tc.errs) {
					return c, tc.errs[attempts-1]
				}
				return c, nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %t", err, tc.wantErr)
			}
			if attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if terminated != tc.wantTerminated {
				t.Errorf("terminated = %d, want %d", terminated, tc.wantTerminated)
			}
		})
	}
}

func TestStartContainer_StopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	_, err := startContainer(t, ctx, func(context.Context) (*fakeContainer, error) {
		attempts++
		cancel()
		return nil, errReaperRace
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}
