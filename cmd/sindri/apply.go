package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/reconcile"
)

// errDrift makes `sindri diff` exit non-zero when the server differs from the
// manifests, so CI can gate on it.
var errDrift = errors.New("drift detected")

// clientFlags locate the server and the credential `apply` and `diff` use.
// The token is read from the environment only: a flag would put it in the
// process list and the shell history.
type clientFlags struct {
	path   string
	server string
}

func (f *clientFlags) register(cmd *cobra.Command) {
	def := os.Getenv("SINDRI_SERVER")
	if def == "" {
		def = "http://localhost:8080"
	}
	cmd.Flags().StringVarP(&f.path, "file", "f", "", "manifest directory, manifest file, or sindri.yaml (required)")
	cmd.Flags().StringVar(&f.server, "server", def, "server URL (default $SINDRI_SERVER or http://localhost:8080)")
	_ = cmd.MarkFlagRequired("file")
}

func newApplyCmd() *cobra.Command {
	var (
		flags clientFlags
		prune bool
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Converge a running server onto manifests",
		Long: `Validate manifests locally, send them to the server, and converge its database
onto them. This is the CI trigger of the one reconciler; serve --config is the
other (ADR 7).

A config-managed repository in scope that the manifests no longer list is
reported as orphaned. --prune deletes orphans; without it nothing is deleted.
The credential is $SINDRI_TOKEN: the bootstrap token, a machine token with the
admin role, or an identity provider token.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := sendManifests(cmd.Context(), cmd.ErrOrStderr(), flags, "apply", prune)
			if err != nil {
				return err
			}
			printResult(cmd.OutOrStdout(), res, false)
			return nil
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&prune, "prune", false, "delete config-managed repositories the manifests no longer list")
	return cmd
}

func newDiffCmd() *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Report how a running server differs from manifests",
		Long: `Report drift between manifests and the server without changing anything. It
is a dry run of apply on the server, so the two cannot disagree. Exits 1 when
anything differs, orphans included.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := sendManifests(cmd.Context(), cmd.ErrOrStderr(), flags, "diff", false)
			if err != nil {
				return err
			}
			printResult(cmd.OutOrStdout(), res, true)
			if res.Drift() {
				return errDrift
			}
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}

func sendManifests(ctx context.Context, stderr io.Writer, f clientFlags, action string, prune bool) (reconcile.Result, error) {
	var res reconcile.Result
	src, err := loadSource(f.path)
	if err != nil {
		return res, reportConfigErrors(stderr, err)
	}
	if src.manifests == nil {
		return res, fmt.Errorf("%s declares no repos, lifecycle, or policy to apply", f.path)
	}
	// Validate before sending, so a mistake is reported with this machine's
	// paths. The server validates again: it never trusts a client.
	if err := reconcile.ValidateSet(src.manifests, reconcile.ValidateOptions{}); err != nil {
		return res, reportConfigErrors(stderr, err)
	}

	body, err := json.Marshal(src.manifests)
	if err != nil {
		return res, fmt.Errorf("encoding manifests: %w", err)
	}
	endpoint, err := url.JoinPath(f.server, "/api/v1/config/", action)
	if err != nil {
		return res, fmt.Errorf("--server %q: %w", f.server, err)
	}
	if prune {
		endpoint += "?prune=true"
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return res, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("SINDRI_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("contacting %s: %w", f.server, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return res, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb api.ErrorBody
		if json.Unmarshal(raw, &eb) != nil || eb.Code == "" {
			return res, fmt.Errorf("server answered %s", resp.Status)
		}
		if len(eb.Details) > 0 {
			return res, reportConfigErrors(stderr, config.Errors(eb.Details))
		}
		return res, fmt.Errorf("server refused the %s: %s: %s", action, eb.Code, eb.Message)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("decoding response: %w", err)
	}
	return res, nil
}

func printResult(w io.Writer, res reconcile.Result, dryRun bool) {
	verb := func(done, would string) string {
		if dryRun {
			return would
		}
		return done
	}
	ch := res.Repositories
	if res.OrgPolicy {
		fmt.Fprintf(w, "%-10s OrgPolicy\n", verb("updated", "update"))
	}
	if res.Stages != nil {
		fmt.Fprintf(w, "%-10s stages [%s] -> [%s]\n", verb("updated", "update"),
			strings.Join(res.Stages.Before, " "), strings.Join(res.Stages.After, " "))
	}
	for _, name := range ch.Created {
		fmt.Fprintf(w, "%-10s %s\n", verb("created", "create"), name)
	}
	for _, u := range ch.Updated {
		for _, f := range u.Fields {
			fmt.Fprintf(w, "%-10s %s: %s %q -> %q\n", verb("updated", "update"), u.Name, f.Field, f.Before, f.After)
		}
	}
	for _, name := range ch.Pruned {
		fmt.Fprintf(w, "%-10s %s\n", "pruned", name)
	}
	pruned := map[string]bool{}
	for _, n := range ch.Pruned {
		pruned[n] = true
	}
	for _, name := range ch.Orphaned {
		if !pruned[name] {
			fmt.Fprintf(w, "%-10s %s (no longer in the manifests; apply --prune deletes it)\n", "orphaned", name)
		}
	}
	if !res.Drift() {
		fmt.Fprintf(w, "no changes: %d repositories match\n", len(ch.Unchanged))
		return
	}
	fmt.Fprintf(w, "%d unchanged\n", len(ch.Unchanged))
}
