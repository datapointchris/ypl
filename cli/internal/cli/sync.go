package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// defaultRuns is how many runs a bare list reads. The collection pages over the
// network and gains a row on every sync, so it is bounded rather than read
// whole.
const defaultRuns = 20

func (a *app) syncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sync",
		Short:   "What the server's sync has been doing",
		GroupID: groupServer,
		Long: "The sync runs on the server, on its own schedule. Nothing here starts one —\n" +
			"these commands read what it has done.",
		RunE: requireSubcommand,
	}
	cmd.AddCommand(a.syncRunsCommand())
	return cmd
}

func (a *app) syncRunsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "The sync's own history",
		RunE:  requireSubcommand,
	}
	cmd.AddCommand(a.syncRunsListCommand())
	return cmd
}

func (a *app) syncRunsListCommand() *cobra.Command {
	var (
		limit  = defaultRuns
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:     "list [flags]",
		Short:   "List the newest sync runs with their failures",
		Example: "  ypl sync runs list\n  ypl sync runs list --limit 5 --json",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			runs, err := client.ListSyncRuns(cmd.Context(), limit)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), runs.Rows)
			}
			if len(runs.Rows) == 0 {
				nothing(cmd, "The server has not synced yet. `ypl status` says what it holds meanwhile.")
				return nil
			}
			printSyncRuns(cmd.OutOrStdout(), runs.Rows)
			if runs.More {
				nothing(cmd, fmt.Sprintf("Older runs follow. `ypl sync runs list --limit %d` reads further back.", limit*2))
			}
			return nil
		},
	}
	addLimit(cmd, &limit, 0, "How many runs to read, over as many pages as that takes")
	addJSON(cmd, &asJSON, "the runs")
	return cmd
}

func printSyncRuns(out io.Writer, runs []api.SyncRun) {
	rows := make([][]string, len(runs))
	for i, run := range runs {
		rows[i] = []string{
			strconv.FormatInt(run.ID, 10),
			run.FinishedTs,
			run.Outcome,
			strconv.FormatInt(run.Playlists, 10),
			"+" + strconv.FormatInt(run.ItemsAdded, 10) + " -" + strconv.FormatInt(run.ItemsRemoved, 10),
			strconv.FormatInt(run.VideoReads, 10),
			strconv.FormatInt(run.TracksFound, 10),
			strconv.FormatInt(run.Units, 10),
			strconv.Itoa(len(run.Failures)),
		}
	}
	table(out, []string{"RUN", "FINISHED", "OUTCOME", "PLAYLISTS", "ITEMS", "READ", "TRACKS", "UNITS", "FAILURES"}, rows)
}
