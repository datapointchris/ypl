package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

func (a *app) playlistsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "playlists",
		Short:   "The playlists on the channel",
		GroupID: groupPlaylists,
		Long: "Name a playlist by its title, its slug from Tab, or its id.\n" +
			"`show` also takes part of a title; a change takes the whole of it.\n" +
			"A change happens on YouTube at once, except an edit, which the next sync pushes.",
		RunE: requireSubcommand,
	}
	splitReadingFromChanging(cmd)
	cmd.AddCommand(
		a.playlistsListCommand(),
		a.playlistsShowCommand(),
		a.playlistsCreateCommand(),
		a.playlistsRenameCommand(),
		a.playlistsDeleteCommand(),
		a.playlistsEditCommand(),
	)
	return cmd
}

func (a *app) playlistsListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		GroupID: groupReading,
		Short:   "List every playlist and its video count",
		Long: "PLAYLIST is the name to type, the one Tab offers: `ypl play be-happy`.\n" +
			"READ counts the videos the server has read for a tracklist.\n" +
			"UNAVAILABLE counts the videos YouTube will not serve, deleted or made private.",
		Example: "  ypl playlists list         every playlist\n" +
			"  ypl playlists list --json  the same, for a script",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			playlists, err := client.ListPlaylists(cmd.Context())
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), playlists)
			}
			if len(playlists) == 0 {
				nothing(cmd, "The server holds no playlists. `ypl server status` says when it last synced.")
				return nil
			}
			printPlaylists(cmd.OutOrStdout(), playlists)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the playlists")
	return cmd
}

func (a *app) playlistsShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "show <playlist>",
		GroupID: groupReading,
		Short:   "Show a playlist's videos, in order",
		Example: "  ypl playlists show sunday-morning  its videos, in the order they play\n" +
			"  ypl playlists show morning         part of a title is enough",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: onlyFirst(a.completePlaylists),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			playlist, err := client.GetPlaylist(cmd.Context(), api.Reference(args[0]))
			if err != nil {
				return reported(namingPlaylists(cmd.Context(), client, err))
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), playlist)
			}
			printPlaylist(cmd.OutOrStdout(), a.width(cmd.OutOrStdout()), playlist)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the playlist")
	return cmd
}

// printPlaylists leads with the handle, since that is what `ypl play` and the
// next command are given without quoting, and Tab offers the same one.
func printPlaylists(out io.Writer, playlists []api.PlaylistSummary) {
	typed := handles(playlists)
	rows := make([][]string, len(playlists))
	for i, playlist := range playlists {
		rows[i] = []string{
			typed[i],
			playlist.Title,
			strconv.FormatInt(playlist.ItemCount, 10),
			strconv.FormatInt(playlist.EnrichedCount, 10),
			strconv.FormatInt(playlist.UnavailableCount, 10),
			playlist.Privacy,
			playlist.ID,
		}
	}
	table(out, 0, []column{whole("PLAYLIST"), whole("TITLE"), whole("VIDEOS"), whole("READ"), whole("UNAVAILABLE"), whole("PRIVACY"), whole("ID")}, rows)
}

func printPlaylist(out io.Writer, width int, playlist api.Playlist) {
	_, _ = fmt.Fprintf(out, "%s  %s\n", playlist.Title, playlist.ID)
	if playlist.Description != "" {
		_, _ = fmt.Fprintln(out, playlist.Description)
	}
	_, _ = fmt.Fprintf(out, "%s, %d read for a tracklist, %d unavailable, %s\n\n",
		count(playlist.ItemCount, "video"), playlist.EnrichedCount, playlist.UnavailableCount, playlist.Privacy)

	rows := make([][]string, len(playlist.Items))
	for i, item := range playlist.Items {
		video := item.Video
		rows[i] = []string{
			strconv.FormatInt(item.Position+1, 10),
			video.ID,
			video.Title,
			video.ChannelTitle,
			clock(video.DurationSeconds),
			strconv.FormatInt(video.TrackCount, 10),
			state(video),
		}
	}
	table(out, width, []column{whole("#"), whole("VIDEO"), prose("TITLE"), detail("CHANNEL"), whole("LENGTH"), whole("TRACKS"), whole("STATE")}, rows)
}

// state is what is worth saying about a video beyond its own fields: that
// YouTube will not serve it, or that its tracklist has not been read.
func state(video api.VideoSummary) string {
	switch {
	case video.IsUnavailable:
		return "unavailable"
	case video.EnrichedTs == nil:
		return "not read yet"
	}
	return ""
}
