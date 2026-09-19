package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

func (a *app) videosCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "videos",
		Short:   "Every video across all playlists",
		GroupID: groupVideos,
		RunE:    requireSubcommand,
	}
	cmd.AddCommand(a.videosListCommand(), a.videosShowCommand(), a.videosSortsCommand())
	return cmd
}

func (a *app) videosListCommand() *cobra.Command {
	var (
		filter     api.VideoFilter
		minMinutes int64
		maxMinutes int64
		limit      int
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List videos, filtered by length, artist or playlist",
		Example: "  ypl videos list --min-minutes 180         videos three hours long or more\n" +
			"  ypl videos list --artist bjork            videos whose tracklist names Björk\n" +
			"  ypl videos list --sort newest --limit 10  the ten latest uploads\n" +
			"  ypl videos list --json                    the whole library, for a script",
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
			read := api.Page[api.LibraryVideo]{}
			if cmd.Flags().Changed("limit") {
				read, err = client.ListVideosUpTo(cmd.Context(), filter, limit)
			} else {
				read.Rows, err = client.ListVideos(cmd.Context(), filter)
			}
			if err != nil {
				return reported(namingPlaylists(cmd.Context(), client, err))
			}
			switch {
			case asJSON:
				err = emitJSON(cmd.OutOrStdout(), read.Rows)
			case len(read.Rows) == 0:
				nothing(cmd, "No video matches. `ypl videos list` with no flags is the whole library.")
			default:
				printVideos(cmd.OutOrStdout(), a.width(cmd.OutOrStdout()), read.Rows)
			}
			if read.More {
				nothing(cmd, "More videos follow; a larger --limit reads them.")
			}
			return err
		},
	}
	cmd.Flags().StringVar(&filter.Playlist, "playlist", "", "Only the videos this playlist holds, by title or id")
	cmd.Flags().StringVar(&filter.Artist, "artist", "", "Only videos whose tracklist names an artist holding this, ignoring case and accents")
	addMinutes(cmd, "min-minutes", &minMinutes, "Only videos at least this many minutes long; a video whose length is unknown is left out")
	addMinutes(cmd, "max-minutes", &maxMinutes, "Only videos at most this many minutes long; a video whose length is unknown is left out")
	cmd.Flags().StringVar(&filter.Sort, "sort", "", "The order: "+strings.Join(api.VideoSorts, ", ")+" (default "+api.VideoSorts[0]+"); newest and oldest go by upload date, and random draws at most "+strconv.Itoa(api.PageSize))
	addLimit(cmd, &limit, 0, "How many videos to list, in the order asked for; every one when not given")
	completeFlag(cmd, "playlist", a.completePlaylists)
	completeFlag(cmd, "sort", cobra.FixedCompletions(api.VideoSorts, cobra.ShellCompDirectiveNoFileComp))
	addJSON(cmd, &asJSON, "the videos")
	return cmd
}

func (a *app) videosShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <video>",
		Short: "Show a video's tracklist, by its link, id or title",
		Long: "Name the video by its link, its id, its title, or part of the title.\n" +
			"FROM is the part of the video each track was read from.",
		Example: "  ypl videos show dQw4w9WgXcQ         the video, track by track\n" +
			"  ypl videos show 'mayan warrior'     by part of its title\n" +
			"  ypl videos show dQw4w9WgXcQ --json  the same, for a script",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			ref := args[0]
			if id := youtube.VideoID(ref); id != "" {
				ref = id
			}
			video, err := client.GetVideo(cmd.Context(), ref)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), video)
			}
			printVideo(cmd.OutOrStdout(), a.width(cmd.OutOrStdout()), video)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the video")
	return goclikit.WithRecoveryHints(cmd, hintVideos)
}

// videosSortsCommand puts the vocabulary under the resource whose flag takes
// it, so the list answers what it is a list of without being run.
func (a *app) videosSortsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "sorts",
		Short:   "List the orders `ypl videos list --sort` takes",
		Example: "  ypl videos sorts  one order a line, for a script",
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

func printVideos(out io.Writer, width int, videos []api.LibraryVideo) {
	rows := make([][]string, len(videos))
	for i, video := range videos {
		rows[i] = []string{
			video.ID,
			video.Title,
			video.ChannelTitle,
			clock(video.DurationSeconds),
			strconv.FormatInt(video.TrackCount, 10),
			firstArtists(video.Artists),
		}
	}
	table(out, width, []column{whole("VIDEO"), prose("TITLE"), detail("CHANNEL"), whole("LENGTH"), whole("TRACKS"), detail("ARTISTS")}, rows)
}

// shownArtists is how many artists a row of the library names. A mix's
// tracklist can name forty, and a row that long wraps every row after it.
const shownArtists = 3

// firstArtists is the first shownArtists of artists, and how many more there
// are. --json carries every one.
func firstArtists(artists []string) string {
	if len(artists) <= shownArtists {
		return strings.Join(artists, ", ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(artists[:shownArtists], ", "), len(artists)-shownArtists)
}

func printVideo(out io.Writer, width int, video api.Video) {
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
	table(out, width, []column{whole("#"), whole("AT"), prose("ARTIST"), prose("TITLE"), whole("FROM")}, rows)
	if len(video.Tracks) == 0 {
		_, _ = fmt.Fprintln(out, "The server read this video and found no tracklist in it.")
	}
}
