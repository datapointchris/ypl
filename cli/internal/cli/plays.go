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
		GroupID: groupPlays,
		Long: "Every listen the server has been told about, newest first. A play is named\n" +
			"by its handle, by its id, or by the last eight characters of that id.",
		RunE: requireSubcommand,
	}
	splitReadingFromChanging(cmd)
	cmd.AddCommand(a.playsListCommand(), a.playsShowCommand(), a.playsAddCommand(), a.playsDeleteCommand())
	return cmd
}

func (a *app) playsDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete <play>",
		GroupID: groupChanging,
		Short:   "Take back a play, so its mix ranks as if unheard by it",
		Long: "Deletes a play the server holds, named by its handle, its id or the last\n" +
			"eight characters of that id. `ypl next` and a bare `ypl play` then rank the\n" +
			"mix as though that listen had not happened, which is how a play `ypl play`\n" +
			"recorded wrongly is taken back.\n" +
			"\n" +
			"It asks first, and needs --yes where there is nobody to ask. A play already\n" +
			"deleted is reported as such, and the command succeeds. The handle is never\n" +
			"given to another play, so a handle from an old listing finds nothing rather\n" +
			"than a different listen.",
		Example: "  ypl plays delete 41        ask, then delete it\n" +
			"  ypl plays delete 41 --yes  delete it without asking",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked before anything is read, so a caller who could never
			// have answered is told what they left out rather than told that a
			// delete did not happen.
			if !yes {
				if err := a.confirmable(cmd); err != nil {
					return err
				}
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			// Read first, so the question names the listen about to go, and
			// the delete names it by its id, so a handle cannot reach a
			// different play between the question and the answer.
			play, err := client.GetPlay(cmd.Context(), args[0])
			if api.PlayDeleted(err) {
				nothing(cmd, fmt.Sprintf("Play %s was already deleted.", args[0]))
				return nil
			}
			if err != nil {
				return reported(err)
			}
			said := fmt.Sprintf("play %d, %s at %s", play.Handle, play.Video.Title, play.PlayedTs)
			if !yes {
				approved, err := a.confirm(cmd, "Delete "+said+"?")
				if err != nil {
					return err
				}
				if !approved {
					nothing(cmd, "Nothing was deleted.")
					return exitCode(1)
				}
			}
			if err := client.DeletePlay(cmd.Context(), play.ID); err != nil {
				return reported(err)
			}
			nothing(cmd, "Deleted "+said+".")
			return nil
		},
	}
	addYes(cmd, &yes, "delete it")
	addNoInput(cmd, "refuse rather than ask, unless --yes answers")
	return goclikit.WithRecoveryHints(cmd, hintPlays)
}

func (a *app) playsAddCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "add <video>",
		GroupID: groupChanging,
		Short:   "Record that a video was listened to",
		Long: "What `ypl next` reads to stop suggesting the same mix. `ypl play` records\n" +
			"what it plays on its own; this is for a mix heard where it could not see, in\n" +
			"a browser or on a phone.\n" +
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
		Use:     "list",
		GroupID: groupReading,
		Short:   "List the newest plays",
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
		GroupID: groupReading,
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
	return goclikit.WithRecoveryHints(cmd, hintPlays)
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
