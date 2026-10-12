package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/gc"
)

func newGCCmd() *cobra.Command {
	var (
		server  string
		dryRun  bool
		timeout time.Duration
		poll    time.Duration
	)
	def := os.Getenv("SINDRI_SERVER")
	if def == "" {
		def = "http://localhost:8080"
	}
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collect blobs nothing references on a running server",
		Long: `Collect garbage on a running server (ADR 19).

A blob is kept while a tag, a PyPI file, a proxy cache entry, evidence, or a
lifecycle event reaches it, directly or through the manifests above it, and
while it was seen within worker.gc.grace. Everything else is deleted: an
untagged, unpromoted image goes, and so do layers no manifest references.

--dry-run lists what a collection would delete now and changes nothing.
Without it the collection is queued for the server's worker and this command
waits for it to finish. The credential is $SINDRI_TOKEN and must be an
administrator's.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := gcClient{server: server, token: os.Getenv("SINDRI_TOKEN"), out: cmd.OutOrStdout()}
			if dryRun {
				return c.dryRun(cmd.Context())
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return c.collect(ctx, poll)
		},
	}
	cmd.Flags().StringVar(&server, "server", def, "server URL (default $SINDRI_SERVER or http://localhost:8080)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted and change nothing")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the collection")
	cmd.Flags().DurationVar(&poll, "poll", time.Second, "how often to check on the collection")
	_ = cmd.Flags().MarkHidden("poll")
	return cmd
}

type gcClient struct {
	server string
	token  string
	out    io.Writer
}

// unclaimedWarning is how long a queued collection may sit before the
// command says that no worker seems to be running.
const unclaimedWarning = 30 * time.Second

func (c gcClient) dryRun(ctx context.Context) error {
	var total, bytes int64
	cursor := ""
	for {
		path := "/api/v1/gc/candidates?limit=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []struct {
				Digest     string `json:"digest"`
				Size       int64  `json:"size"`
				LastSeenAt string `json:"last_seen_at"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.call(ctx, http.MethodGet, path, http.StatusOK, &page); err != nil {
			return err
		}
		for _, it := range page.Items {
			fmt.Fprintf(c.out, "%s  %10s  last seen %s\n", it.Digest, humanBytes(it.Size), it.LastSeenAt)
			total++
			bytes += it.Size
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	fmt.Fprintf(c.out, "would delete %d blobs, %s\n", total, humanBytes(bytes))
	return nil
}

type gcJob struct {
	ID        int64     `json:"id"`
	State     string    `json:"state"`
	LastError string    `json:"last_error"`
	Result    gc.Result `json:"result"`
}

func (c gcClient) collect(ctx context.Context, poll time.Duration) error {
	var job gcJob
	if err := c.call(ctx, http.MethodPost, "/api/v1/gc", http.StatusAccepted, &job); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "collection queued as job %d\n", job.ID)

	queued := time.Now()
	warned := false
	for {
		switch job.State {
		case "succeeded":
			r := job.Result
			fmt.Fprintf(c.out, "deleted %d blobs, %s, of %d candidates; skipped %d recent, %d referenced, %d in use\n",
				r.Deleted, humanBytes(r.BytesFreed), r.Candidates, r.SkippedRecent, r.SkippedReferenced, r.SkippedLocked)
			if r.Errors > 0 {
				return fmt.Errorf("%d blobs could not be deleted; the server log names them, and the next run retries them", r.Errors)
			}
			return nil
		case "failed":
			return fmt.Errorf("collection job %d failed: %s", job.ID, job.LastError)
		case "queued":
			if !warned && time.Since(queued) > unclaimedWarning {
				fmt.Fprintln(c.out, "still queued: no worker has claimed it; is worker.enabled false on every server?")
				warned = true
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for collection job %d: %w; it keeps running on the server", job.ID, ctx.Err())
		case <-time.After(poll):
		}
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", job.ID), http.StatusOK, &job); err != nil {
			return err
		}
	}
}

func (c gcClient) call(ctx context.Context, method, path string, want int, out any) error {
	base, err := url.Parse(c.server)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("--server %q is not a URL such as http://localhost:8080", c.server)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.server, "/")+path, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", c.server, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != want {
		var eb api.ErrorBody
		if json.Unmarshal(raw, &eb) == nil && eb.Code != "" {
			return fmt.Errorf("server answered %s: %s: %s", resp.Status, eb.Code, eb.Message)
		}
		return fmt.Errorf("server answered %s", resp.Status)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
