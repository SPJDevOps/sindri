package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "sindri",
		Short:         "A lightweight multi-protocol artifact repository",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newVersionCmd(), newServeCmd(), newConfigCmd(), newApplyCmd(), newDiffCmd(), newGCCmd())
	return cmd
}
