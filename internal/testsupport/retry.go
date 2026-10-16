//go:build integration

package testsupport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// startAttempts bounds how often startContainer tries.
const startAttempts = 3

// startContainer runs start, retrying a fresh container when the attempt
// failed for a reason that belongs to Docker under parallel load rather than
// to the container. Both such failures were seen under `go test ./...`, which
// runs each package as its own process:
//
//   - The processes share one reaper. When one has created the reaper
//     container but not yet started it, another finds it, sees status
//     "created", and testcontainers gives up rather than retrying (v0.44.0).
//     A moment later the reaper is running and the same call succeeds.
//   - Docker Desktop never published a started container's port mapping, so
//     the port wait timed out on a healthy Postgres. Waiting longer on that
//     container does not help; a new one gets a new mapping.
//
// A failed attempt's container is terminated here; on error the caller gets
// the zero C and nothing to clean up.
func startContainer[C testcontainers.Container](t testing.TB, ctx context.Context, start func(context.Context) (C, error)) (C, error) {
	t.Helper()
	var zero C
	for attempt := 1; ; attempt++ {
		c, err := start(ctx)
		if err == nil {
			return c, nil
		}
		if terr := testcontainers.TerminateContainer(c); terr != nil {
			err = errors.Join(err, terr)
		}
		if !isTransientStart(err) || attempt == startAttempts {
			return zero, err
		}

		pause := time.Duration(attempt) * 500 * time.Millisecond
		t.Logf("container start failed for a transient reason (attempt %d of %d), retrying in %s: %v",
			attempt, startAttempts, pause, err)
		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("retrying container start: %w", errors.Join(ctx.Err(), err))
		case <-time.After(pause):
		}
	}
}

// isTransientStart reports whether err is one of the failures startContainer
// retries. testcontainers exports no sentinel or type for either, so the
// wrapping it applies is the only signal: "reaper: " and "reaper connect: "
// from obtaining the reaper, "mapped port: " from waiting on a port mapping.
func isTransientStart(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "reaper") || strings.Contains(msg, "mapped port")
}
