package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/datapointchris/goclikit"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// defaultPlays is how many plays a bare list reads. The collection pages over
// the network and grows for as long as anything is listened to, so it is
// bounded rather than read whole.
const defaultPlays = 20

func (a *app) playsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plays",
		Short:   "What has been listened to",
		GroupID: groupLibrary,
		Long: "Every listen the server has been told about, newest first. A play is named\n" +
			"by its handle, by its id, or by the last eight characters of that id.",
		RunE: requireSubcommand,
	}
	cmd.AddCommand(a.playsListCommand(), a.playsShowCommand(), a.playsAddCommand())
	return cmd
}

func (a *app) playsAddCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "add <video>",
		Short: "Record that a video was listened to",
		Long: "What `ypl next` reads to stop suggesting the same mix. Written when a listen\n" +
			"is logged rather than inferred from playback, because `ypl playlists play` hands\n" +
			"mpv the whole playlist at once and never learns which of it got played.\n" +
			"\n" +
			"The video is named by its id or by a URL it was copied from. The server takes\n" +
			"the moment the request arrived as when it was played.",
		Example: "  ypl plays add dQw4w9WgXcQ                             log one by id\n" +
			"  ypl plays add 'https://youtu.be/dQw4w9WgXcQ?t=42'     log one from a link",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			videoID := youtube.VideoID(args[0])
			if videoID == "" {
				return goclikit.UsageError(fmt.Errorf("%q is not a video id or a YouTube address", args[0]))
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			// The id is made here rather than by the server, so a play sent
			// again after an answer went missing is stored once rather than
			// twice. Re-running the command is a different listen and makes a
			// new one.
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("make an id for the play: %w", err)
			}
			play, err := client.CreatePlay(cmd.Context(), api.PlayID(id.String()), api.VideoID(videoID))
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), play)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  %s\n", play.Handle, play.PlayedTs, play.Video.Title)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the play")
	return cmd
}

func (a *app) playsListCommand() *cobra.Command {
	var (
		limit  = defaultPlays
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list [flags]",
		Short: "List the newest plays",
		Example: "  ypl plays list                     what has been on lately\n" +
			"  ypl plays list --limit 100 --json  further back, for a script",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			plays, err := client.ListPlays(cmd.Context(), limit)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), plays.Rows)
			}
			if len(plays.Rows) == 0 {
				nothing(cmd, "Nothing has been listened to yet. `ypl next` picks something to put on.")
				return nil
			}
			printPlays(cmd.OutOrStdout(), plays.Rows)
			if plays.More {
				nothing(cmd, fmt.Sprintf("More plays follow. `ypl plays list --limit %d` reads further back.", limit*2))
			}
			return nil
		},
	}
	addLimit(cmd, &limit, 0, "How many plays to read, over as many pages as that takes")
	addJSON(cmd, &asJSON, "the plays")
	return cmd
}

func (a *app) playsShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "show <play>",
		Short:   "Show one play",
		Example: "  ypl plays show 41  when this one was played, and what it was",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			play, err := client.GetPlay(cmd.Context(), args[0])
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), play)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d  %s\n%s\n%s\n%s\n",
				play.Handle, play.PlayedTs, play.Video.Title, play.Video.ChannelTitle, play.ID)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the play")
	return cmd
}

// printPlays leads with the handle, since that is what `ypl plays show` is
// given and a UUID is not something anyone retypes.
func printPlays(out io.Writer, plays []api.Play) {
	rows := make([][]string, len(plays))
	for i, play := range plays {
		rows[i] = []string{
			strconv.FormatInt(play.Handle, 10),
			play.PlayedTs,
			play.Video.ID,
			play.Video.Title,
			play.Video.ChannelTitle,
		}
	}
	table(out, []string{"PLAY", "WHEN", "VIDEO", "TITLE", "CHANNEL"}, rows)
}
