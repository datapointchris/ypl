package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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
			"one last_ok_run. The day's YouTube quota and each playlist waiting to be\n" +
			"pushed to YouTube are under sync.\n\n" +
			"Exits 1 when the server has not finished an ok sync in the last hour, or in\n" +
			"twelve of its ticks where those are longer, so a check on a timer can run\n" +
			"it as it is.",
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
			now := time.Now()
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			} else {
				printStatus(cmd.OutOrStdout(), status, now)
			}
			if stale(status, now) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The server has not finished an ok sync in the last %s.\n", window(staleAfter(status)))
				return exitCode(1)
			}
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
		Long: "OUTCOME is ok, what fell short, or why the sync stopped; one that is not ok\n" +
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

func printStatus(out io.Writer, status api.Status, now time.Time) {
	library := status.Library
	_, _ = fmt.Fprintf(out, "%s, %s, %s, %s\n",
		count(library.Playlists, "playlist"), count(library.Videos, "video"),
		count(library.Tracks, "track"), count(library.Plays, "play"))
	if held := library.VideosWithTracklist; held != nil {
		_, _ = fmt.Fprintf(out, "%s with a tracklist, %d read for one, %d unavailable\n",
			count(*held, "video"), library.EnrichedVideos, library.UnavailableVideos)
	} else {
		_, _ = fmt.Fprintf(out, "%s read for a tracklist, %d unavailable\n",
			count(library.EnrichedVideos, "video"), library.UnavailableVideos)
	}
	_, _ = fmt.Fprintf(out, "%s\n\n", backlog(status))

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
	quota := status.Sync.Quota
	if quota.UnitsLimit > 0 {
		_, _ = fmt.Fprintf(out, "Quota         %d of %d units spent today%s\n", quota.UnitsSpent, quota.UnitsLimit, resets(quota, now))
	}
	for i, push := range status.Sync.Pushes {
		label := "To push     "
		if i > 0 {
			label = "            "
		}
		line := fmt.Sprintf("%s  %s, %s", label, push.Title, count(push.Writes, "write"))
		if push.Held {
			line += ", waiting on a write YouTube refused today"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	if misses := status.Sync.ProbeMissesLastDay; misses > 0 {
		_, _ = fmt.Fprintf(out, "The sweep found %s on YouTube in the last day that the probe missed.\n", count(misses, "changed playlist"))
	}
}

// resets is when the quota resets, as how long from now, and nothing where
// the server did not say.
func resets(quota api.Quota, now time.Time) string {
	at, err := time.Parse(time.RFC3339, quota.ResetsAt)
	if err != nil || !at.After(now) {
		return ""
	}
	left := at.Sub(now).Round(time.Minute)
	return fmt.Sprintf(", resets in %dh %02dm", int(left.Hours()), int(left.Minutes())%60)
}

// staleAfter is how long after its last ok sync a server reads as stale: an
// hour, or twelve of its ticks where those are longer.
func staleAfter(status api.Status) time.Duration {
	return max(time.Hour, 12*time.Duration(status.Sync.IntervalSeconds)*time.Second)
}

// window is d as the end of "in the last ...": "hour", "3 hours", "90m0s".
func window(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "hour"
	case d%time.Hour == 0:
		return count(int64(d/time.Hour), "hour")
	}
	return d.String()
}

// stale is whether the server has finished no ok sync within staleAfter of
// now.
func stale(status api.Status, now time.Time) bool {
	if status.LastOKRun == nil {
		return true
	}
	finished, err := time.Parse(time.RFC3339, status.LastOKRun.FinishedTs)
	return err != nil || now.Sub(finished) > staleAfter(status)
}

// backlog is how many videos still wait for a tracklist read, and how long
// reading at the last hour's pace takes. The count is videos less those read
// and those YouTube will not serve, so it is close rather than exact: a video
// can be both read and unavailable.
func backlog(status api.Status) string {
	library := status.Library
	left := max(0, library.Videos-library.EnrichedVideos-library.UnavailableVideos)
	if left == 0 {
		return "Every video has been read for a tracklist."
	}
	line := fmt.Sprintf("About %s not yet read for a tracklist", count(left, "video"))
	if pace := status.Sync.TracklistReadsLastHour; pace > 0 {
		line += fmt.Sprintf(", about %s at %d an hour", count((left+pace-1)/pace, "hour"), pace)
	}
	return line + "."
}

// outcome is how a sync ended, with a partial one named by what fell short:
// the tracklist reads that failed out of those tried, the playlists that
// failed, and any other step. A sync is partial exactly when it recorded
// failures and still ran to the end.
func outcome(run api.SyncRun) string {
	if run.Outcome != "partial" {
		return run.Outcome
	}
	var reads, playlists, steps int64
	for _, failure := range run.Failures {
		switch {
		case failure.Stage == "enrichment":
			reads++
		case failure.PlaylistID != nil:
			playlists++
		default:
			steps++
		}
	}
	var parts []string
	if reads > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d reads failed", reads, run.VideoReads))
	}
	if playlists > 0 {
		parts = append(parts, count(playlists, "playlist")+" failed")
	}
	if steps > 0 {
		parts = append(parts, count(steps, "sync step")+" failed")
	}
	if len(parts) == 0 {
		return run.Outcome
	}
	return strings.Join(parts, ", ")
}

// syncLine is one sync as a line: when it finished, how it ended, and what it
// changed, read and spent.
func syncLine(run api.SyncRun) string {
	line := fmt.Sprintf("%s  %s  %d playlists, +%d -%d playlist videos, %d quota units",
		run.FinishedTs, outcome(run), run.Playlists, run.ItemsAdded, run.ItemsRemoved, run.Units)
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
			outcome(run),
			strconv.FormatInt(run.Playlists, 10),
			"+" + strconv.FormatInt(run.ItemsAdded, 10) + " -" + strconv.FormatInt(run.ItemsRemoved, 10),
			strconv.FormatInt(run.VideoReads, 10),
			strconv.FormatInt(run.TracksFound, 10),
			strconv.FormatInt(run.Units, 10),
			strconv.Itoa(len(run.Failures)),
		}
	}
	table(out, 0, []column{
		whole("SYNC"), whole("FINISHED"), whole("OUTCOME"), whole("PLAYLISTS"), whole("CHANGED"),
		whole("READS"), whole("TRACKS"), whole("QUOTA"), whole("FAILURES"),
	}, rows)
	for _, run := range runs {
		if len(run.Failures) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(out, "\nSync %d, %s:\n", run.ID, outcome(run))
		for _, failure := range run.Failures {
			_, _ = fmt.Fprintf(out, "  %s %s: %s\n", failure.Stage, failedOn(failure), failure.Error)
		}
	}
}
