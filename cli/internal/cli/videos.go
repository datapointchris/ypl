package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

func (a *app) videosCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "videos",
		Short:   "The mixes across every playlist",
		GroupID: groupReading,
		Long: "Every available video some playlist holds, with the artists its tracklist\n" +
			"names and the playlists it is in. This is the library as one set, rather\n" +
			"than a playlist at a time.",
		RunE: requireSubcommand,
	}
	cmd.AddCommand(a.videosListCommand(), a.videosShowCommand(), a.videosSortsCommand())
	return cmd
}

func (a *app) videosListCommand() *cobra.Command {
	var (
		filter     api.VideoFilter
		minMinutes int64
		maxMinutes int64
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "list [flags]",
		Short: "List the library, narrowed and ordered",
		Example: "  ypl videos list --artist bjork\n" +
			"  ypl videos list --playlist 'sunday morning' --sort longest\n" +
			"  ypl videos list --min-minutes 90 --json",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if filter.Sort != "" && !slices.Contains(api.VideoSorts, filter.Sort) {
				return usageError{fmt.Errorf("sort %q is not one of %s", filter.Sort, strings.Join(api.VideoSorts, ", "))}
			}
			filter.MinSeconds, filter.MaxSeconds = asSeconds(minMinutes), asSeconds(maxMinutes)
			if filter.MinSeconds >= 0 && filter.MaxSeconds >= 0 && filter.MinSeconds > filter.MaxSeconds {
				return usageError{fmt.Errorf("--min-minutes %d is more than --max-minutes %d", minMinutes, maxMinutes)}
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			videos, err := client.ListVideos(cmd.Context(), filter)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), videos)
			}
			printVideos(cmd.OutOrStdout(), videos)
			return nil
		},
	}
	cmd.Flags().StringVar(&filter.Playlist, "playlist", "", "Only the videos this playlist holds, by title or id")
	cmd.Flags().StringVar(&filter.Artist, "artist", "", "Only videos whose tracklist names an artist holding this, ignoring case and accents")
	cmd.Flags().Int64Var(&minMinutes, "min-minutes", -1, "Only videos at least this long")
	cmd.Flags().Int64Var(&maxMinutes, "max-minutes", -1, "Only videos at most this long")
	cmd.Flags().StringVar(&filter.Sort, "sort", "", "The order: "+strings.Join(api.VideoSorts, ", "))
	addJSON(cmd, &asJSON, "the videos")
	return cmd
}

func (a *app) videosShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "show <video-id>",
		Short:   "Show one video with its tracklist",
		Example: "  ypl videos show dQw4w9WgXcQ\n  ypl videos show dQw4w9WgXcQ --json",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			video, err := client.GetVideo(cmd.Context(), args[0])
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), video)
			}
			printVideo(cmd.OutOrStdout(), video)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the video")
	return cmd
}

// videosSortsCommand puts the vocabulary under the resource whose flag takes
// it, so the list answers what it is a list of without being run.
func (a *app) videosSortsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "sorts",
		Short:   "List the orders --sort accepts",
		Example: "  ypl videos sorts",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), api.VideoSorts)
			}
			for _, name := range api.VideoSorts {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the orders")
	return cmd
}

// asSeconds is a bound given in minutes as the seconds the server takes, and a
// negative for a bound that was not given.
func asSeconds(minutes int64) int64 {
	if minutes < 0 {
		return -1
	}
	return minutes * 60
}

func printVideos(out io.Writer, videos []api.LibraryVideo) {
	rows := make([][]string, len(videos))
	for i, video := range videos {
		rows[i] = []string{
			video.ID,
			video.Title,
			video.ChannelTitle,
			clock(video.DurationSeconds),
			strconv.FormatInt(video.TrackCount, 10),
			strings.Join(video.Artists, ", "),
		}
	}
	table(out, []string{"VIDEO", "TITLE", "CHANNEL", "LENGTH", "TRACKS", "ARTISTS"}, rows)
}

func printVideo(out io.Writer, video api.Video) {
	_, _ = fmt.Fprintf(out, "%s  %s\n", video.Title, video.ID)
	_, _ = fmt.Fprintf(out, "%s", video.ChannelTitle)
	if length := clock(video.DurationSeconds); length != "" {
		_, _ = fmt.Fprintf(out, "  %s", length)
	}
	if uploaded := text(video.UploadDate); uploaded != "" {
		_, _ = fmt.Fprintf(out, "  uploaded %s", uploaded)
	}
	_, _ = fmt.Fprintln(out)
	if playlists := len(video.Playlists); playlists > 0 {
		titles := make([]string, playlists)
		for i, playlist := range video.Playlists {
			titles[i] = playlist.Title
		}
		_, _ = fmt.Fprintf(out, "In %s\n", strings.Join(titles, ", "))
	}
	if video.EnrichedTs == nil {
		_, _ = fmt.Fprintln(out, "\nNo tracklist read yet — the server reads one on a later run.")
		return
	}
	_, _ = fmt.Fprintln(out)

	rows := make([][]string, len(video.Tracks))
	for i, track := range video.Tracks {
		rows[i] = []string{
			strconv.FormatInt(track.Position, 10),
			clock(track.StartSeconds),
			text(track.Artist),
			track.Title,
			track.Source,
		}
	}
	table(out, []string{"#", "AT", "ARTIST", "TITLE", "FROM"}, rows)
	if len(video.Tracks) == 0 {
		_, _ = fmt.Fprintln(out, "The server read this video and found no tracklist in it.")
	}
}
