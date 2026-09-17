package cli

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/editbuffer"
	"github.com/datapointchris/ypl/cli/internal/mpv"
)

// asked is what `ypl now` reads from mpv. time-pos is what puts a position
// inside a tracklist, and the other two are what a video the server has never
// heard of is reported by.
var asked = []string{"path", "time-pos", "duration", "media-title"}

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
		Short:   "What is playing right now, down to the track",
		GroupID: groupPlaying,
		Long: "Reads the socket `ypl play` opened. Because the server holds a tracklist with\n" +
			"real timestamps, this reports the track inside a two-hour mix rather than the\n" +
			"name of the mix.\n" +
			"\n" +
			"Exits 1 when nothing is playing, so a status bar can run it unguarded.",
		Example: "  ypl now         what is on, and how far in\n" +
			"  ypl now --json  the same, for a status bar",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := mpv.Properties(mpv.SocketPath(), asked)
			if errors.Is(err, mpv.ErrNotPlaying) {
				nothing(cmd, "Nothing is playing. `ypl play <playlist>` puts something on.")
				return exitCode(1)
			}
			if err != nil {
				return reported(err)
			}

			found := nowPlaying{
				VideoID:         editbuffer.VideoID(asText(state["path"])),
				Title:           asText(state["media-title"]),
				PositionSeconds: asSeconds(state["time-pos"]),
				DurationSeconds: asSeconds(state["duration"]),
			}
			// What mpv is playing is only sometimes something the server knows
			// about. A video that is not in the library still answers, from
			// mpv's own title, rather than reporting that nothing is on.
			if found.VideoID != "" {
				client, err := a.client(cmd.Context())
				if err != nil {
					return reported(err)
				}
				video, err := client.GetVideo(cmd.Context(), found.VideoID)
				var refusal *api.Refusal
				switch {
				case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
				case err != nil:
					return reported(err)
				default:
					found.Title, found.Channel = video.Title, video.ChannelTitle
					if video.DurationSeconds != nil {
						found.DurationSeconds = video.DurationSeconds
					}
					found.Track = trackAt(video.Tracks, found.PositionSeconds)
				}
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

// trackAt is the track position falls in, and nil where the tracklist has none
// that holds it.
//
// A track with no start is skipped rather than matched: its place in the
// tracklist is known and the moment it begins is not, so nothing can say
// whether the player is inside it.
func trackAt(tracks []api.Track, position *int64) *api.Track {
	if position == nil {
		return nil
	}
	for i, track := range tracks {
		if track.StartSeconds == nil || *track.StartSeconds > *position {
			continue
		}
		// The end is what the next track's start says where a tracklist carries
		// no end of its own, and the last track runs to the end of the video.
		if track.EndSeconds != nil && *track.EndSeconds <= *position {
			continue
		}
		if track.EndSeconds == nil && i+1 < len(tracks) {
			if next := tracks[i+1].StartSeconds; next != nil && *next <= *position {
				continue
			}
		}
		return &tracks[i]
	}
	return nil
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

// asText is a property mpv answered with as a string, and "" for one it did not
// answer at all.
func asText(value any) string {
	text, _ := value.(string)
	return text
}

// asSeconds is a property mpv answered with as whole seconds. mpv sends a
// position as a float, and nothing here shows anything finer than a second.
func asSeconds(value any) *int64 {
	seconds, ok := value.(float64)
	if !ok {
		return nil
	}
	whole := int64(seconds)
	return &whole
}
