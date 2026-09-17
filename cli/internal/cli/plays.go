package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// defaultPlays is how many plays a bare list reads. The collection pages over
// the network and grows for as long as anything is listened to, so it is
// bounded rather than read whole.
const defaultPlays = 20

func (a *app) playsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plays",
		Short:   "What has been listened to",
		GroupID: groupReading,
		Long: "Every listen the server has been told about, newest first. A play is named\n" +
			"by its handle, by its id, or by the last eight characters of that id.",
		RunE: requireSubcommand,
	}
	cmd.AddCommand(a.playsListCommand(), a.playsShowCommand())
	return cmd
}

func (a *app) playsListCommand() *cobra.Command {
	var (
		limit  = defaultPlays
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:     "list [flags]",
		Short:   "List the newest plays",
		Example: "  ypl plays list\n  ypl plays list --limit 100 --json",
		Args:    usageArgs(cobra.NoArgs),
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
		Example: "  ypl plays show 41\n  ypl plays show 41 --json",
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
