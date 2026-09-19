package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// defaultSyncs is how many syncs a bare list reads. The collection pages over
// the network and gains a row with every sync, so it is bounded rather than
// read whole.
const defaultSyncs = 20

func (a *app) serverCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Short:   "What the server holds, and how its syncs with YouTube went",
		GroupID: groupServer,
		Long:    "The server syncs with YouTube on its own schedule. These commands only read.",
		RunE:    requireSubcommand,
	}
	cmd.AddCommand(a.serverStatusCommand(), a.serverSyncsCommand())
	return cmd
}

func (a *app) serverStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what the server holds and how its last sync went",
		Long: "A video with a tracklist holds at least one track, videos_with_tracklist in\n" +
			"--json. One read for a tracklist has been searched for its tracks, whether or\n" +
			"not it held any, enriched_videos. The last sync is last_run, and the last ok\n" +
			"one last_ok_run.",
		Example: "  ypl server status         the library's size and the last sync\n" +
			"  ypl server status --json  the same, for a check on a timer",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			status, err := client.GetStatus(cmd.Context())
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), status)
			}
			printStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the status")
	return cmd
}

func (a *app) serverSyncsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "syncs",
		Short: "The server's syncs with YouTube",
		RunE:  requireSubcommand,
	}
	cmd.AddCommand(a.serverSyncsListCommand())
	return cmd
}

func (a *app) serverSyncsListCommand() *cobra.Command {
	var (
		limit  = defaultSyncs
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent syncs: what each changed, read and spent",
		Long: "OUTCOME is ok, or the word for why the sync fell short; one that is not ok\n" +
			"has its failures listed below the table. CHANGED is videos added to and\n" +
			"removed from playlists, items_added and items_removed in --json. READS is\n" +
			"tracklist reads attempted, video_reads. QUOTA is the YouTube Data API units\n" +
			"the sync spent, units.",
		Example: fmt.Sprintf("  ypl server syncs list                   the last %d\n", defaultSyncs) +
			"  ypl server syncs list --limit 5 --json  the last five, for a script",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			syncs, err := client.ListSyncRuns(cmd.Context(), limit)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), syncs.Rows)
			}
			if len(syncs.Rows) == 0 {
				nothing(cmd, "The server has not synced yet. `ypl server status` says what it holds meanwhile.")
				return nil
			}
			printSyncs(cmd.OutOrStdout(), syncs.Rows)
			if syncs.More {
				nothing(cmd, fmt.Sprintf("Older syncs follow: `ypl server syncs list --limit %d`.", limit*2))
			}
			return nil
		},
	}
	addLimit(cmd, &limit, 0, "How many syncs to list")
	addJSON(cmd, &asJSON, "the syncs")
	return cmd
}

func printStatus(out io.Writer, status api.Status) {
	library := status.Library
	_, _ = fmt.Fprintf(out, "%s, %s, %s, %s\n",
		count(library.Playlists, "playlist"), count(library.Videos, "video"),
		count(library.Tracks, "track"), count(library.Plays, "play"))
	if held := library.VideosWithTracklist; held != nil {
		_, _ = fmt.Fprintf(out, "%s with a tracklist, %d read for one, %d unavailable\n\n",
			count(*held, "video"), library.EnrichedVideos, library.UnavailableVideos)
	} else {
		_, _ = fmt.Fprintf(out, "%s read for a tracklist, %d unavailable\n\n",
			count(library.EnrichedVideos, "video"), library.UnavailableVideos)
	}

	if status.LastRun == nil {
		_, _ = fmt.Fprintln(out, "The server has not synced yet.")
		return
	}
	_, _ = fmt.Fprintf(out, "Last sync     %s\n", syncLine(*status.LastRun))
	switch {
	case status.LastOKRun == nil:
		_, _ = fmt.Fprintln(out, "Last ok sync  never")
	case status.LastOKRun.ID != status.LastRun.ID:
		_, _ = fmt.Fprintf(out, "Last ok sync  %s\n", syncLine(*status.LastOKRun))
	}
	for _, failure := range status.LastRun.Failures {
		_, _ = fmt.Fprintf(out, "  %s %s: %s\n", failure.Stage, failedOn(failure), failure.Error)
	}
}

// syncLine is one sync as a line: when it finished, how it ended, and what it
// changed, read and spent.
func syncLine(run api.SyncRun) string {
	line := fmt.Sprintf("%s  %s  %d playlists, +%d -%d playlist videos, %d quota units",
		run.FinishedTs, run.Outcome, run.Playlists, run.ItemsAdded, run.ItemsRemoved, run.Units)
	if run.VideoReads > 0 {
		line += fmt.Sprintf(", %d tracklist reads found %d tracks", run.VideoReads, run.TracksFound)
	}
	if run.EnrichmentPaused {
		line += ", reading paused"
	}
	if run.IsRateLimited {
		line += ", rate limited"
	}
	return line
}

// failedOn is what a failure was about, and "the sync" for one about the sync
// itself.
func failedOn(failure api.SyncFailure) string {
	switch {
	case failure.PlaylistID != nil:
		return *failure.PlaylistID
	case failure.VideoID != nil:
		return *failure.VideoID
	}
	return "the sync"
}

func printSyncs(out io.Writer, runs []api.SyncRun) {
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
	table(out, []string{"SYNC", "FINISHED", "OUTCOME", "PLAYLISTS", "CHANGED", "READS", "TRACKS", "QUOTA", "FAILURES"}, rows)
	for _, run := range runs {
		if len(run.Failures) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(out, "\nSync %d, %s:\n", run.ID, run.Outcome)
		for _, failure := range run.Failures {
			_, _ = fmt.Fprintf(out, "  %s %s: %s\n", failure.Stage, failedOn(failure), failure.Error)
		}
	}
}
