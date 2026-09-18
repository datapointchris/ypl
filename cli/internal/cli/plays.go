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
		Short:   "What you have listened to",
		GroupID: groupPlays,
		Long: "`ypl play` records a video once it has played long enough.\n" +
			"Name a play by the number in the PLAY column of `ypl plays list`.",
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
		Short:   "Delete a play recorded by mistake",
		Long: "Name the play by the number in the PLAY column of `ypl plays list`.\n" +
			"It asks first; --yes answers for it. The video then ranks as if that play\n" +
			"never happened.",
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
		Short:   "Record a video heard somewhere else, by its link or id",
		Long: "`ypl play` records what it plays. This is for a video heard in a browser or on\n" +
			"a phone, and records it as heard now.",
		Example: "  ypl plays add dQw4w9WgXcQ                             record one by id\n" +
			"  ypl plays add 'https://youtu.be/dQw4w9WgXcQ?t=42'     record one from a link",
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
		Short:   "List recent plays, newest first",
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
