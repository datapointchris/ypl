package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

func (a *app) statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "What the server holds, and how its last sync went",
		GroupID: groupServer,
		Long: "The size of the library, the newest sync run, and the newest run that ended\n" +
			"ok. The two runs differ when the latest one failed, and the gap between\n" +
			"them is how long the mirror has been going stale.",
		Example: "  ypl status         is the server reachable, and is the mirror current\n" +
			"  ypl status --json  the same, for a check that runs on a timer",
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

func printStatus(out io.Writer, status api.Status) {
	library := status.Library
	_, _ = fmt.Fprintf(out, "%s, %s, %s, %s\n",
		count(library.Playlists, "playlist"), count(library.Videos, "video"),
		count(library.Tracks, "track"), count(library.Plays, "play"))
	_, _ = fmt.Fprintf(out, "%d with a tracklist, %d unavailable\n\n",
		library.EnrichedVideos, library.UnavailableVideos)

	if status.LastRun == nil {
		_, _ = fmt.Fprintln(out, "The server has not synced yet.")
		return
	}
	_, _ = fmt.Fprintf(out, "Last run     %s\n", runLine(*status.LastRun))
	switch {
	case status.LastOKRun == nil:
		_, _ = fmt.Fprintln(out, "Last ok      never")
	case status.LastOKRun.ID != status.LastRun.ID:
		_, _ = fmt.Fprintf(out, "Last ok      %s\n", runLine(*status.LastOKRun))
	}
	for _, failure := range status.LastRun.Failures {
		_, _ = fmt.Fprintf(out, "  %s %s: %s\n", failure.Stage, failedOn(failure), failure.Error)
	}
}

// runLine is one run as a line: when it finished, how it ended, and what it
// moved.
func runLine(run api.SyncRun) string {
	line := fmt.Sprintf("%s  %s  %d playlists, +%d -%d items, %d units",
		run.FinishedTs, run.Outcome, run.Playlists, run.ItemsAdded, run.ItemsRemoved, run.Units)
	if run.VideoReads > 0 {
		line += fmt.Sprintf(", %d videos read for %d tracks", run.VideoReads, run.TracksFound)
	}
	if run.EnrichmentPaused {
		line += ", reading paused"
	}
	if run.IsRateLimited {
		line += ", rate limited"
	}
	return line
}

// failedOn is what a failure was about, and "" for one about the run itself.
func failedOn(failure api.SyncFailure) string {
	switch {
	case failure.PlaylistID != nil:
		return *failure.PlaylistID
	case failure.VideoID != nil:
		return *failure.VideoID
	}
	return "the run"
}
