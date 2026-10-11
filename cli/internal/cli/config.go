package cli

import (
	"errors"
	"fmt"

	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/goclilogin"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/config"
)

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "Where this machine finds the server",
		GroupID: groupSetup,
		Long: "The server's address, its identity provider and this machine's client, from\n" +
			"one config file. `ypl auth login <url>` fills in whatever it lacks.",
	}
	goclikit.AsNamespace(cmd)
	cmd.AddCommand(newConfigShowCommand(), newConfigPathCommand())
	return cmd
}

func newConfigShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show each setting and where it came from",
		Example: "  ypl config show         each setting, and whether the file or a flag set it\n" +
			"  ypl config show --json  the same, for a script",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			deployment, err := config.Load(cmd.Context(), apiOverride)
			if err != nil && !errors.Is(err, goclilogin.ErrNotConfigured) {
				return err
			}
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), deployment); err != nil {
					return err
				}
			} else {
				goclilogin.WriteDeployment(cmd.OutOrStdout(), deployment)
			}
			if err != nil {
				nothing(cmd, "\n"+err.Error())
				return exitCode(1)
			}
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the settings")
	return cmd
}

func newConfigPathCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "path",
		Short:   "Print the config file's path",
		Example: "  ypl config path  where the file goes, whether or not it exists",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := config.Path()
			if path == "" {
				return errors.New("no config file: neither XDG_CONFIG_HOME nor a home directory is set")
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}
