package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/config"
)

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "Where this machine finds the server",
		GroupID: groupSetup,
		Long: "The server's address and its identity provider, from the config file or\n" +
			"YPL_API_BASE and YPL_OIDC_ISSUER.",
		RunE: requireSubcommand,
	}
	cmd.AddCommand(newConfigShowCommand(), newConfigPathCommand(), newConfigExampleCommand())
	return cmd
}

func newConfigExampleCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "example",
		Short:   "Print a config file to fill in",
		Example: "  ypl config example > \"$(ypl config path)\"  write it, then fill it in",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), config.Example())
			return nil
		},
	}
}

// resolvedSetting is one row of `ypl config show`, carrying where the value
// came from. A stale export and a line in the config file produce the same
// value, and only the layer tells them apart.
type resolvedSetting struct {
	Key   string       `json:"key"`
	Value string       `json:"value"`
	From  config.Layer `json:"from"`
	Env   string       `json:"env"`
}

// resolvedConfig is what `ypl config show --json` writes.
type resolvedConfig struct {
	Path     string            `json:"path"`
	Settings []resolvedSetting `json:"settings"`
}

func newConfigShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show each setting and where it came from",
		Example: "  ypl config show         each setting, and whether a file or a variable set it\n" +
			"  ypl config show --json  the same, for a script",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			resolved := resolvedConfig{Path: cfg.Path}
			for _, setting := range cfg.Settings {
				resolved.Settings = append(resolved.Settings, resolvedSetting{
					Key: setting.Key, Value: setting.Value, From: setting.Layer, Env: setting.Env,
				})
			}
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), resolved); err != nil {
					return err
				}
			} else {
				printConfig(cmd.OutOrStdout(), resolved)
			}
			if len(cfg.Missing()) > 0 {
				nothing(cmd, "\n"+cfg.Check().Error())
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
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}

func printConfig(out io.Writer, resolved resolvedConfig) {
	_, _ = fmt.Fprintf(out, "%s\n\n", resolved.Path)
	rows := make([][]string, len(resolved.Settings))
	for i, setting := range resolved.Settings {
		rows[i] = []string{setting.Key, setting.Value, string(setting.From), setting.Env}
	}
	table(out, 0, []column{whole("SETTING"), whole("VALUE"), whole("FROM"), whole("ENVIRONMENT")}, rows)
}
