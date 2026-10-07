package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/reconcile"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Validate and inspect configuration without a server",
	}
	cmd.AddCommand(newConfigCheckCmd(), newConfigShowCmd())
	return cmd
}

func newConfigCheckCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate a sindri.yaml or a manifest directory statically",
		Long: `Validate configuration with no database and no server, so it can run in CI
before anything is deployed. Every problem is reported, each naming the file,
the line, and the field. Exits non-zero when anything is wrong.

-f takes a sindri.yaml, a manifest file, or a directory of manifests. A
manifest directory's credentials references are checked by the server when it
is applied, because the secrets live there.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := loadSource(path)
			if err == nil {
				err = src.validate()
			}
			if err != nil {
				return reportConfigErrors(cmd.ErrOrStderr(), err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: ok (%s)\n", path, src.summary())
			return err
		},
	}
	cmd.Flags().StringVarP(&path, "file", "f", config.DefaultPath, "sindri.yaml, a manifest file, or a manifest directory")
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	var (
		path     string
		expanded bool
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print configuration as the server sees it",
		Long: `Print the configuration after loading, with every secret redacted.

With --expanded, desired state is printed in the long form: every inferred
type and protocol, every preset and default written out, and every member
reference qualified with its type. This is the form the reconciler acts on, and
the first thing to reach for when resolution order is not what was expected.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := loadSource(path)
			if err != nil {
				return reportConfigErrors(cmd.ErrOrStderr(), err)
			}
			return src.show(cmd.OutOrStdout(), expanded)
		},
	}
	cmd.Flags().StringVarP(&path, "file", "f", config.DefaultPath, "sindri.yaml, a manifest file, or a manifest directory")
	cmd.Flags().BoolVar(&expanded, "expanded", false, "print desired state in the desugared long form")
	return cmd
}

// configSource is what -f named: a sindri.yaml, or long-form manifests.
type configSource struct {
	server    *config.Config // nil for manifests
	manifests *config.Manifests
}

// loadSource reads a sindri.yaml or manifests, telling them apart by shape so
// one flag serves both.
func loadSource(path string) (*configSource, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !info.IsDir() {
		raw, err := os.ReadFile(path) //nolint:gosec // the operator names their own configuration file
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if !config.LooksLikeManifests(raw) {
			cfg, err := config.Parse(raw, path)
			if err != nil {
				return nil, err
			}
			return &configSource{server: cfg, manifests: cfg.Desired}, nil
		}
	}
	m, err := config.LoadManifests(path)
	if err != nil {
		return nil, err
	}
	return &configSource{manifests: m}, nil
}

func (s *configSource) validate() error {
	if s.manifests == nil {
		return nil
	}
	opts := reconcile.ValidateOptions{}
	if s.server != nil {
		// A sindri.yaml carries its own credentials section, so references
		// can be checked here rather than at reconcile time.
		opts.Credentials = s.server.Credentials.Names()
	}
	return reconcile.ValidateSet(s.manifests, opts)
}

func (s *configSource) summary() string {
	if s.manifests == nil {
		return "server settings only, no desired state"
	}
	stages := 0
	if s.manifests.Lifecycle != nil {
		stages = len(s.manifests.Lifecycle.Stages)
	}
	return fmt.Sprintf("%d repositories, %d stages", len(s.manifests.Repositories), stages)
}

func (s *configSource) show(w io.Writer, expanded bool) error {
	if s.server != nil {
		raw, err := config.MarshalServer(s.server)
		if err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return fmt.Errorf("writing configuration: %w", err)
		}
	}
	if !expanded || s.manifests == nil {
		return nil
	}
	raw, err := config.MarshalManifests(s.manifests)
	if err != nil {
		return err
	}
	if s.server != nil && len(raw) > 0 {
		if _, err := io.WriteString(w, "---\n"); err != nil {
			return fmt.Errorf("writing configuration: %w", err)
		}
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("writing configuration: %w", err)
	}
	return nil
}

// errReported means the details were already written; main prints only this.
var errReported = errors.New("configuration is invalid")

func reportConfigErrors(w io.Writer, err error) error {
	es := config.AsErrors(err)
	for _, e := range es {
		fmt.Fprintln(w, e.Error())
	}
	if len(es) == 1 {
		return fmt.Errorf("%w: 1 problem", errReported)
	}
	return fmt.Errorf("%w: %d problems", errReported, len(es))
}
