package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// nowPlaying is what `ypl now` answers.
type nowPlaying struct {
	VideoID string `json:"video_id"`
	Title   string `json:"title"`
	Channel string `json:"channel_title"`
	// PositionSeconds is where mpv is in the video, and null before it has
	// started.
	PositionSeconds *int64 `json:"position_seconds"`
	DurationSeconds *int64 `json:"duration_seconds"`
	// Track is the track of the tracklist this position falls in, and null
	// where the video has no tracklist or the position is in no track of it.
	Track *api.Track `json:"track"`
}

func (a *app) nowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "now",
		Short:   "Show the video and the track playing now",
		GroupID: groupPlaying,
		Long:    "Exits 1 when nothing is playing.",
		Example: "  ypl now         what is on, and how far in\n" +
			"  ypl now --json  the same, for a status bar",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			found, err := a.readNow(cmd.Context(), a.client)
			switch {
			case errors.Is(err, mpv.ErrNotPlaying):
				// Render first, decide the exit code after, in both modes. A
				// status bar reads the empty answer off the code, and --json is
				// the rendering it parses, so writing nothing here would hand
				// the only caller that depends on it a parse error.
				if asJSON {
					if err := emitJSON(cmd.OutOrStdout(), nowPlaying{}); err != nil {
						return err
					}
				}
				nothing(cmd, nothingPlaying)
				return exitCode(1)
			case err != nil:
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), found)
			}
			printNow(cmd, found)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "what is playing")
	return cmd
}

// nothingPlaying is what `ypl now` and a bare `ypl` say with nothing on.
const nothingPlaying = "Nothing is playing. `ypl play` plays what you have heard least lately,\n" +
	"and `ypl play <playlist>` one playlist, which Tab completes."

// readNow is what mpv is playing, with the server's title and the track at
// mpv's position where the server knows the video. It is mpv.ErrNotPlaying
// where nothing is.
//
// The client is asked for only once mpv has answered, so nothing playing
// costs no config, no keychain and no request.
func (a *app) readNow(ctx context.Context, client func(context.Context) (*api.Client, error)) (nowPlaying, error) {
	state, err := mpv.Read(mpv.SocketPath())
	if err != nil {
		// A socket that answered and could not be read is not nothing
		// playing, and is returned as itself. Reporting it as nothing would
		// make the command wrong about the one thing it is asked.
		return nowPlaying{}, err
	}
	found := nowPlaying{
		VideoID:         youtube.VideoID(state.Path),
		Title:           state.Title,
		PositionSeconds: state.Position,
		DurationSeconds: state.Duration,
	}
	// What mpv is playing is only sometimes something the server knows
	// about. A video that is not in the library still answers, from mpv's own
	// title, rather than reporting that nothing is on.
	if found.VideoID == "" {
		return found, nil
	}
	reached, err := client(ctx)
	if err != nil {
		return nowPlaying{}, err
	}
	video, err := reached.GetVideo(ctx, found.VideoID)
	var refusal *api.Refusal
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
	case err != nil:
		return nowPlaying{}, err
	default:
		found.Title, found.Channel = video.Title, video.ChannelTitle
		if video.DurationSeconds != nil {
			found.DurationSeconds = video.DurationSeconds
		}
		found.Track = trackAt(video.Tracks, found.PositionSeconds)
	}
	return found, nil
}

// trackAt is the track position falls in, and nil where the tracklist has none
// that holds it.
//
// The latest-starting track that holds the position wins, rather than the first
// one found. A track whose start the store does not carry sits between two that
// do, and taking the first would leave the track before it bounded by nothing
// and reported for every later position. Scanning for the latest needs no
// assumption about the order the tracks arrive in either.
//
// A track with no start is never the answer. Its place in the tracklist is
// known and the moment it begins is not, so nothing can say whether the player
// is inside it.
func trackAt(tracks []api.Track, position *int64) *api.Track {
	if position == nil {
		return nil
	}
	found := -1
	for i, track := range tracks {
		if track.StartSeconds == nil || *track.StartSeconds > *position {
			continue
		}
		// An end the tracklist carries is where the track stops, so a position
		// on it belongs to whatever comes next.
		if track.EndSeconds != nil && *track.EndSeconds <= *position {
			continue
		}
		if found < 0 || *track.StartSeconds > *tracks[found].StartSeconds {
			found = i
		}
	}
	if found < 0 {
		return nil
	}
	return &tracks[found]
}

func printNow(cmd *cobra.Command, found nowPlaying) {
	out := cmd.OutOrStdout()
	if found.Track != nil {
		name := found.Track.Title
		if found.Track.Artist != nil && *found.Track.Artist != "" {
			name = *found.Track.Artist + " - " + name
		}
		_, _ = fmt.Fprintln(out, name)
	}
	_, _ = fmt.Fprintf(out, "%s  %s / %s\n", found.Title, clock(found.PositionSeconds), clock(found.DurationSeconds))
	if found.Channel != "" {
		_, _ = fmt.Fprintln(out, found.Channel)
	}
	if found.Track == nil && found.VideoID != "" {
		nothing(cmd, "No track for this position. `ypl videos show <video>` says whether its tracklist has been read.")
	}
}
