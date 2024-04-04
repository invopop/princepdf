package main

import (
	"github.com/spf13/cobra"
)

type rootOpts struct {
}

func root() *rootOpts {
	return &rootOpts{}
}

func (o *rootOpts) cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           name,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// cmd.AddCommand(versionCmd())
	cmd.AddCommand(serve(o).cmd())

	return cmd
}
