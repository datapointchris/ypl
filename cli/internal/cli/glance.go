package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
)

// glance is what a bare `ypl` answers with: what is playing, what the server
// holds and how its last sync ended, and the one command to run next.
//
// Not the catalog. Somebody typing a bare command is asking where things stand
// and what to do about it; `ypl --help` answers what the tool can do, and the
// last line names it.
//
// A machine that cannot reach the library yet — no config, no login — gets
// the refusal every other command gives, since that sentence already names
// the command that fixes it.
func (a *app) glance(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	client, err := a.client(cmd.Context())
	if err != nil {
		return reported(err)
	}
	reached := func(context.Context) (*api.Client, error) { return client, nil }

	// Both reads land before anything is written, so a server that refuses
	// the second leaves stdout empty rather than holding half an answer.
	found, err := a.readNow(cmd.Context(), reached)
	playing := !errors.Is(err, mpv.ErrNotPlaying)
	if playing && err != nil {
		return reported(err)
	}
	status, err := client.GetStatus(cmd.Context())
	if err != nil {
		return reported(err)
	}

	if playing {
		printNow(cmd, found)
	} else {
		_, _ = fmt.Fprintln(out, nothingPlaying)
	}
	library := status.Library
	_, _ = fmt.Fprintf(out, "\n%s, %s, %d with a tracklist.\n",
		count(library.Playlists, "playlist"), count(library.Videos, "video"), library.EnrichedVideos)
	switch run := status.LastRun; {
	case run == nil:
		_, _ = fmt.Fprintln(out, "The server has not synced yet.")
	case run.Outcome == "ok":
		_, _ = fmt.Fprintf(out, "Last synced %s.\n", run.FinishedTs)
	default:
		_, _ = fmt.Fprintf(out, "The last sync, at %s, ended %s. `ypl server status` says what it hit.\n", run.FinishedTs, run.Outcome)
	}
	_, _ = fmt.Fprintln(out, "\n`ypl help` lists every command.")
	return nil
}
