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

	"github.com/datapointchris/ypl/cli/internal/api"
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
	return newRootCommand(&app{client: newAPIClient, tokens: goclilogin.NewTokenStore, terminal: isTerminal})
}

func newRootCommand(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "ypl",
		Short: "The ypl command-line client",
		Long: "ypl plays YouTube playlists of long DJ mixes, over a server that keeps the\n" +
			"playlists mirrored and reads a tracklist for each mix.\n" +
			"\n" +
			"What you do while listening sits at the top: play, now, next. The library is\n" +
			"noun then verb, so moving from reading a playlist to acting on it changes\n" +
			"only the final word. Every read takes --json.\n" +
			"\n" +
			"Tab completes a playlist wherever one is named, as its title slugged — `ypl\n" +
			"play sunday-morning` plays Sunday Morning. The title works as well, and its\n" +
			"case, spacing and punctuation do not have to be reproduced. A read takes part\n" +
			"of a title too; a verb that changes something does not, because a fragment\n" +
			"matching one playlist matches it unambiguously.\n" +
			"\n" +
			"A bare `ypl` says what is playing and where the server stands. Run any other\n" +
			"partial command with no arguments or --help to see what comes next.",
		Example: "  ypl play <Tab>                             pick a playlist and play it\n" +
			"  ypl play                                   play a draw, least recently heard first\n" +
			"  ypl now                                    which track of the mix is on\n" +
			"  ypl playlists list                         every playlist, and how much of it is read\n" +
			"  ypl config example > \"$(ypl config path)\"  first run: write the file, then fill it in\n" +
			"  ypl auth login                             log this machine in, once",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without this cobra's own root validator answers an unknown command
		// with an untyped error, which exits 1 and reads as a failure rather
		// than as a word that is not a command.
		Args: cobra.ArbitraryArgs,
		// The root is the one command that answers bare rather than showing
		// help. It has no sibling a bare invocation could be confused with,
		// and the glance takes no flags and writes nothing.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.glance(cmd)
			}
			return requireSubcommand(cmd, args)
		},
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

	root.AddGroup(
		&cobra.Group{ID: groupPlaying, Title: "Playing:"},
		&cobra.Group{ID: groupLibrary, Title: "The library:"},
		&cobra.Group{ID: groupServer, Title: "The server:"},
		&cobra.Group{ID: groupSetup, Title: "Setting up:"},
	)
	root.AddCommand(
		a.playCommand(),
		a.nowCommand(),
		a.nextCommand(),
		a.playlistsCommand(),
		a.videosCommand(),
		a.playsCommand(),
		a.statusCommand(),
		a.syncCommand(),
		a.authCommand(),
		newConfigCommand(),
		newUpdateCommand(),
	)
	noFiles(root)
	return root
}

// Execute runs the command tree and returns the process exit code, writing
// anything that failed to stderr.
func Execute() int {
	err := executeTree(context.Background(), NewRootCommand(), AutoUpdateConfig())
	report(os.Stderr, err)
	return exitCodeFor(err)
}

// executeTree is everything Execute wraps around the tree, so a test drives the
// path the binary takes with an update check of its own. A test that ran the
// tree bare would miss what goclikit adds to an error on the way out.
func executeTree(ctx context.Context, root *cobra.Command, update autoupdate.Config) error {
	return goclikit.Execute(ctx, root, update, goclikit.WithNotFound(notFound))
}

// Where a not-found sends somebody, attached to the commands whose reference
// the server did not recognize. Each is a sentence, a colon, and the command.
// A playlist has none: its not-found lists every playlist instead, which
// namingPlaylists writes, because the whole set is one request away. A video
// and a play are pointed at, since those sets grow without bound.
const (
	hintVideos = "Every video in the library: `ypl videos list`"
	hintPlays  = "The plays lately: `ypl plays list`"
)

// notFound reports whether err is the server saying nothing answers to a
// reference, and the text the hint goes under.
//
// That text is the whole of err rather than the refusal's own sentence. The
// hint replaces what it is given, and an edit joins its refusal with where the
// buffer was kept; the refusal alone would drop the only copy of the
// rearranging from the terminal. A refusal with no sentence came from
// something in front of the server, a proxy's page, and the resource was never
// reached.
func notFound(err error) (string, bool) {
	var refusal *api.Refusal
	if !errors.As(err, &refusal) || refusal.Message == "" {
		return "", false
	}
	switch refusal.Code {
	case "not_found", "unknown_reference":
		return err.Error(), true
	}
	return "", false
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
