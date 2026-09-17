// Package cli holds the ypl command tree.
package cli

import (
	"github.com/spf13/cobra"
)

// version is set at build time with -ldflags.
var version = "dev"

// NewRootCommand returns the ypl command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "ypl",
		Short:         "The ypl command-line client",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newUpdateCommand())
	return root
}
