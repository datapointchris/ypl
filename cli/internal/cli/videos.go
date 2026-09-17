package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/datapointchris/goclikit"
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
			// Absence comes from the parser, so a bound of zero is a bound
			// rather than a flag nobody set.
			filter.MinSeconds = secondsIn(cmd, "min-minutes", minMinutes)
			filter.MaxSeconds = secondsIn(cmd, "max-minutes", maxMinutes)
			if filter.MinSeconds != nil && filter.MaxSeconds != nil && *filter.MinSeconds > *filter.MaxSeconds {
				return goclikit.UsageError(fmt.Errorf("--min-minutes %d is more than --max-minutes %d", minMinutes, maxMinutes))
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
			if len(videos) == 0 {
				nothing(cmd, "No video matches. `ypl videos list` with no flags is the whole library.")
				return nil
			}
			printVideos(cmd.OutOrStdout(), videos)
			return nil
		},
	}
	cmd.Flags().StringVar(&filter.Playlist, "playlist", "", "Only the videos this playlist holds, by title or id")
	cmd.Flags().StringVar(&filter.Artist, "artist", "", "Only videos whose tracklist names an artist holding this, ignoring case and accents")
	addMinutes(cmd, "min-minutes", &minMinutes, "Only videos at least this many minutes long")
	addMinutes(cmd, "max-minutes", &maxMinutes, "Only videos at most this many minutes long")
	cmd.Flags().StringVar(&filter.Sort, "sort", "", "The order, one of "+strings.Join(api.VideoSorts, ", ")+"; the server decides")
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

// secondsIn is the bound flag names, in the seconds the server takes, and nil
// where it was not given.
func secondsIn(cmd *cobra.Command, flag string, given int64) *int64 {
	if !cmd.Flags().Changed(flag) {
		return nil
	}
	seconds := given * 60
	return &seconds
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
	// A video YouTube will not serve is never read again — the enrichment queue
	// filters it out — so promising a later run would be telling a reader to
	// wait for something that cannot happen.
	if video.IsUnavailable {
		_, _ = fmt.Fprintln(out, "\nYouTube will not serve this video, so the server cannot read a tracklist for it.")
		return
	}
	if video.EnrichedTs == nil {
		_, _ = fmt.Fprintln(out, "\nThe server has not read this one yet.")
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
