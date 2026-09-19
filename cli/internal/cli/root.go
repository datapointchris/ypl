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
	"github.com/spf13/pflag"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// version is set at build time with -ldflags.
var version = "dev"

// The root's help sections. Playing comes first because it is what the tool
// is for, and each library namespace has a section of its own, so its
// commands are not interleaved with another's.
const (
	groupPlaying   = "playing"
	groupPlaylists = "playlists"
	groupVideos    = "videos"
	groupPlays     = "plays"
	groupServer    = "server"
	groupSetup     = "setup"
)

// Within a namespace whose verbs both read and change something, the verbs
// are split by which they do, so the half that changes something is visible
// without reading each Short.
const (
	groupReading  = "reading"
	groupChanging = "changing"
)

// splitReadingFromChanging gives cmd the two sections its verbs go under.
func splitReadingFromChanging(cmd *cobra.Command) {
	cmd.AddGroup(
		&cobra.Group{ID: groupReading, Title: "Reading commands:"},
		&cobra.Group{ID: groupChanging, Title: "Changing commands:"},
	)
}

// NewRootCommand returns the ypl command tree.
func NewRootCommand() *cobra.Command {
	return newRootCommand(&app{client: newAPIClient, tokens: goclilogin.NewTokenStore, terminal: isTerminal, width: widthOf})
}

func newRootCommand(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "ypl",
		Short: "Play and organize YouTube playlists of long DJ mixes",
		Long: "A command is a noun, then a verb, so reading a playlist and changing one differ\n" +
			"only in the last word. A namespace on its own, like `ypl playlists`, lists the\n" +
			"commands under it. Name a playlist by its title, its slug from Tab, or its id.\n" +
			"A read also takes part of a title, and a change does not. Every list and show\n" +
			"takes --json.",
		Example: "  ypl play <Tab>                     play a playlist\n" +
			"  ypl play                           play what you have heard least lately\n" +
			"  ypl now                            which track is playing\n" +
			"  ypl videos list --min-minutes 180  videos three hours long or more\n" +
			"  ypl plays list                     what you have listened to\n" +
			"  ypl server status                  what the server holds, and its last sync",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without this cobra's own root validator answers an unknown command
		// with an untyped error, which exits 1 and reads as a failure rather
		// than as a word that is not a command.
		Args: cobra.ArbitraryArgs,
		RunE: requireSubcommand,
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if id, ok := dashedVideoID(err); ok {
			err = fmt.Errorf("%w; a video id that starts with a dash goes after --: `%s -- %s`", err, cmd.CommandPath(), id)
		}
		return goclikit.UsageError(err)
	})
	useHelp(root)

	// Cobra's automatic version flag claims -v, which is the counted verbosity
	// flag everywhere else. Leave --version long-only rather than teach one
	// letter two jobs.
	root.InitDefaultVersionFlag()
	if flag := root.Flags().Lookup("version"); flag != nil {
		flag.Shorthand = ""
		flag.Usage = "Print which release of ypl this is"
	}

	root.AddGroup(
		&cobra.Group{ID: groupPlaying, Title: "Playing commands:"},
		&cobra.Group{ID: groupPlaylists, Title: "Playlist commands:"},
		&cobra.Group{ID: groupVideos, Title: "Video commands:"},
		&cobra.Group{ID: groupPlays, Title: "Listening history commands:"},
		&cobra.Group{ID: groupServer, Title: "Server commands:"},
		&cobra.Group{ID: groupSetup, Title: "Setup commands:"},
	)
	// Tab is how a playlist is named without quoting it, so the command that
	// sets Tab up is listed with the rest of the setup.
	root.SetCompletionCommandGroupID(groupSetup)
	root.AddCommand(
		a.playCommand(),
		a.nowCommand(),
		a.nextCommand(),
		a.playlistsCommand(),
		a.videosCommand(),
		a.playsCommand(),
		a.serverCommand(),
		a.authCommand(),
		newConfigCommand(),
		newUpdateCommand(),
		moved("status", "ypl server status"),
		moved("sync", "ypl server syncs list"),
	)
	noFiles(root)
	return root
}

// dashedVideoID is the video id a flag parser refused as a flag, and false when
// err refused anything else. A YouTube id can open with a dash, and one copied
// from a list then reads as a run of unknown short flags.
func dashedVideoID(err error) (string, bool) {
	var unknown *pflag.NotExistError
	if !errors.As(err, &unknown) {
		return "", false
	}
	token := "--" + unknown.GetSpecifiedName()
	if shorts := unknown.GetSpecifiedShortnames(); shorts != "" {
		token = "-" + shorts
	}
	return token, youtube.IsVideoID(token)
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
