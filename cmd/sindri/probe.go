package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// newProbeCmd is a healthcheck for images with no shell and no curl, such as
// the distroless one Sindri ships in: Compose and Kubernetes exec it, and it
// exits zero only when the URL answers 2xx.
func newProbeCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "probe URL",
		Short: "Exit zero when URL answers 2xx; for container healthchecks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, args[0], nil)
			if err != nil {
				return fmt.Errorf("probe %q: %w", args[0], err)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("probe %s: %w", args[0], err)
			}
			_ = res.Body.Close()
			if res.StatusCode < 200 || res.StatusCode > 299 {
				return fmt.Errorf("probe %s: %s", args[0], res.Status)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "give up after this long")
	cmd.SilenceUsage = true
	return cmd
}
