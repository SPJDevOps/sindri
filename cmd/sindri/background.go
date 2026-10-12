package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/worker"
)

// Job kinds besides gc.JobKind. Each is also its own dedupe key.
const (
	jobEvictProxyCache = "evict_proxy_cache"
	jobPurgeUploads    = "purge_uploads"
)

// finishedJobRetention is how long a finished job stays readable through
// /api/v1/jobs before the purge job removes it.
const finishedJobRetention = 30 * 24 * time.Hour

// handlers are the jobs this binary knows how to run.
func (a *app) handlers(cfg config.Worker) map[string]worker.Handler {
	return map[string]worker.Handler{
		gc.JobKind: func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			res, err := a.collector.Collect(ctx)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
		jobEvictProxyCache: func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			res, err := a.reg.EvictStaleCache(ctx, cfg.Retention.ProxyStaleAfter.Duration())
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
		jobPurgeUploads: func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			uploads, err := a.blobs.PurgeUploads(ctx, time.Now().Add(-cfg.Retention.UploadMaxIdle.Duration()))
			if err != nil {
				return nil, fmt.Errorf("purging staged uploads: %w", err)
			}
			jobs, err := a.queue.DeleteFinished(ctx, finishedJobRetention)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]int64{"uploads": int64(uploads), "jobs": jobs})
		},
	}
}

func schedules(cfg config.Worker) []worker.Schedule {
	s := []worker.Schedule{
		{Kind: jobEvictProxyCache, Interval: cfg.Retention.Interval.Duration()},
		{Kind: jobPurgeUploads, Interval: cfg.Retention.Interval.Duration()},
	}
	if cfg.GC.Interval > 0 {
		s = append(s, worker.Schedule{Kind: gc.JobKind, Interval: cfg.GC.Interval.Duration()})
	}
	return s
}

// startBackground starts the work that runs beside the HTTP server: the
// worker, when the configuration enables it, and the expiry of idle upload
// sessions held in this process's memory, which always runs because only this
// process can reach them (ADR 20). stop cancels both and waits for them, and
// must run before the database pool closes: a worker stopping mid-job hands
// the job back through the database.
func (a *app) startBackground(parent context.Context, cfg *config.Config, bo buildOptions) (stop func()) {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup

	wg.Go(func() { a.expireUploads(ctx, cfg.Worker.Retention.UploadMaxIdle.Duration()) })

	if cfg.Worker.Enabled {
		if cfg.Worker.GC.Grace.Duration() < time.Hour {
			a.logger.WarnContext(ctx, "worker.gc.grace is under an hour; a push slower than the grace period can lose a layer the moment it is uploaded",
				"grace", cfg.Worker.GC.Grace.String())
		}
		handlers := a.handlers(cfg.Worker)
		for kind, h := range bo.ExtraHandlers {
			handlers[kind] = h
		}
		opts := worker.Options{
			Queue:     a.queue,
			Handlers:  handlers,
			Owner:     workerOwner(),
			Poll:      bo.WorkerPoll,
			Schedules: schedules(cfg.Worker),
			Logger:    a.logger,
		}
		wg.Go(func() {
			if err := worker.Run(ctx, opts); err != nil {
				a.logger.ErrorContext(ctx, "the worker did not start", "error", err.Error())
			}
		})
	} else {
		a.logger.InfoContext(ctx, "worker disabled; queued jobs wait for a worker elsewhere")
	}

	return func() {
		cancel()
		wg.Wait()
	}
}

// expireUploads aborts upload sessions idle longer than maxIdle, checking a
// few times per allowance so an abandoned session is gone soon after it
// qualifies.
func (a *app) expireUploads(ctx context.Context, maxIdle time.Duration) {
	t := time.NewTicker(min(max(maxIdle/4, time.Second), 15*time.Minute))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n := a.cas.ExpireIdle(ctx, time.Now().Add(-maxIdle)); n > 0 {
			a.logger.InfoContext(ctx, "expired idle upload sessions", "sessions", n)
		}
	}
}

// workerOwner names this worker on the leases it holds: the host and process
// for an operator reading the jobs table, and a random suffix so that two
// workers can never share a name.
func workerOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b))
}
