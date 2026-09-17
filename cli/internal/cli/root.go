// Package cli holds the ypl command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/goclilogin"
	"github.com/datapointchris/goselfupdate/autoupdate"
	"github.com/spf13/cobra"
)

// version is set at build time with -ldflags.
var version = "dev"

// The help sections, named for what someone is trying to do rather than for
// what the commands are.
const (
	groupLibrary = "library"
	groupPlaying = "playing"
	groupServer  = "server"
	groupSetup   = "setup"
)

// NewRootCommand returns the ypl command tree.
func NewRootCommand() *cobra.Command {
	return newRootCommand(&app{client: newAPIClient, tokens: goclilogin.NewTokenStore})
}

func newRootCommand(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "ypl",
		Short: "The ypl command-line client",
		Long: "ypl organizes YouTube playlists of long DJ mixes, over a server that keeps\n" +
			"the playlists mirrored and reads a tracklist for each mix.\n" +
			"\n" +
			"The noun comes first and the verb last, so moving from reading a playlist\n" +
			"to acting on it changes only the final word. Every read takes --json.\n" +
			"\n" +
			"A playlist is named by its title as readily as by its YouTube id, and the\n" +
			"title's case, spacing and punctuation do not have to be reproduced — `ypl\n" +
			"playlists show 'sunday morning'` finds Sunday Morning. A read takes part of\n" +
			"a title too; a verb that changes something does not, because a fragment\n" +
			"matching one playlist matches it unambiguously.\n" +
			"\n" +
			"Run any partial command with no arguments or --help to see what comes next.",
		Example: "  ypl config example > \"$(ypl config path)\"  first run: write the file, then fill it in\n" +
			"  ypl auth login                             log this machine in, once\n" +
			"  ypl status                                 check the server is reachable and what it holds\n" +
			"  ypl next                                   pick something to put on",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without this cobra's own root validator answers an unknown command
		// with an untyped error, which exits 1 and reads as a failure rather
		// than as a word that is not a command.
		Args: cobra.ArbitraryArgs,
		RunE: requireSubcommand,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return goclikit.UsageError(err) })
	useUsageTemplate(root)

	// Cobra's automatic version flag claims -v, which is the counted verbosity
	// flag everywhere else. Leave --version long-only rather than teach one
	// letter two jobs.
	root.InitDefaultVersionFlag()
	if flag := root.Flags().Lookup("version"); flag != nil {
		flag.Shorthand = ""
	}

	// Read back off the flag set rather than bound to a variable here, because a
	// variable at this scope is process-wide state and every command in the tree
	// would share one copy of it.
	root.PersistentFlags().Bool(noInput, false,
		"Never prompt; a verb that would have asked for confirmation refuses instead")

	root.AddGroup(
		&cobra.Group{ID: groupLibrary, Title: "The library:"},
		&cobra.Group{ID: groupPlaying, Title: "Playing:"},
		&cobra.Group{ID: groupServer, Title: "The server:"},
		&cobra.Group{ID: groupSetup, Title: "Setting up:"},
	)
	root.AddCommand(
		a.playlistsCommand(),
		a.videosCommand(),
		a.playsCommand(),
		a.nextCommand(),
		a.statusCommand(),
		a.syncCommand(),
		a.authCommand(),
		newConfigCommand(),
		newUpdateCommand(),
	)
	return root
}

// Execute runs the command tree and returns the process exit code, writing
// anything that failed to stderr.
func Execute() int {
	err := goclikit.Execute(context.Background(), NewRootCommand(), AutoUpdateConfig())
	report(os.Stderr, err)
	return exitCodeFor(err)
}

// report writes err as the line a person reads, and writes nothing for the two
// kinds that have already said their piece. An exitCode carries a code and no
// message, because the command printed what it found and an "error:" line over
// it would report a failure that did not happen; `update` writes its own line
// for the same reason.
func report(out io.Writer, err error) {
	var code exitCode
	if err == nil || errors.As(err, &code) || errors.Is(err, goclikit.ErrReported) {
		return
	}
	_, _ = fmt.Fprintln(out, "error:", err)
}

// AutoUpdateConfig is the daily update check that runs beside a command.
func AutoUpdateConfig() autoupdate.Config {
	return autoupdate.Config{Update: updateConfig()}
}
