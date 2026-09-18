package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// The playlist verbs are split by whether they change the channel, so the half
// that writes to YouTube is visible without reading each Short.
const (
	groupPlaylistReading = "playlist-reading"
	groupPlaylistWriting = "playlist-writing"
)

func (a *app) playlistsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "playlists",
		Short:   "The playlists the server mirrors",
		GroupID: groupLibrary,
		Long: "Every playlist on the channel, as the server last read it. A playlist is\n" +
			"named by its title or by its YouTube id wherever one is named, and Tab\n" +
			"completes it.\n" +
			"\n" +
			"The verbs below that change something change it on YouTube. A new playlist\n" +
			"is made there private, and a rename and a delete happen there in the request\n" +
			"that asks for them. An edit is the exception: it changes the order the server\n" +
			"holds, and the next sync run pushes that order to YouTube.",
		RunE: requireSubcommand,
	}
	// Declared here rather than on the root, because delete and edit are the
	// only commands that read it and both are under this one. On the root it
	// prints under Global Flags for every command in the tree, including the
	// ones that never prompt.
	//
	// Read back off the flag set rather than bound to a variable here, because a
	// variable at this scope is process-wide state and every command in the tree
	// would share one copy of it.
	cmd.PersistentFlags().Bool(noInput, false,
		"Never prompt; a verb that would have asked for confirmation refuses instead")

	cmd.AddGroup(
		&cobra.Group{ID: groupPlaylistReading, Title: "Reading:"},
		&cobra.Group{ID: groupPlaylistWriting, Title: "Changing:"},
	)
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
		GroupID: groupPlaylistReading,
		Short:   "List every playlist with what it holds",
		Example: "  ypl playlists list         what is on the channel, and how much of it is read\n" +
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
				nothing(cmd, "The server holds no playlists. `ypl status` says when it last synced.")
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
		GroupID: groupPlaylistReading,
		Short:   "Show one playlist and the videos in it, in order",
		Example: "  ypl playlists show 'sunday morning'  what is in it, in the order it plays\n" +
			"  ypl playlists show morning           part of a title is enough for a read",
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
			printPlaylist(cmd.OutOrStdout(), playlist)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the playlist")
	return cmd
}

// printPlaylists puts the title first, since a title is what the next command
// is given.
func printPlaylists(out io.Writer, playlists []api.PlaylistSummary) {
	rows := make([][]string, len(playlists))
	for i, playlist := range playlists {
		rows[i] = []string{
			playlist.Title,
			strconv.FormatInt(playlist.ItemCount, 10),
			strconv.FormatInt(playlist.EnrichedCount, 10),
			strconv.FormatInt(playlist.UnavailableCount, 10),
			playlist.Privacy,
			playlist.ID,
		}
	}
	table(out, []string{"TITLE", "VIDEOS", "READ", "GONE", "PRIVACY", "ID"}, rows)
}

func printPlaylist(out io.Writer, playlist api.Playlist) {
	_, _ = fmt.Fprintf(out, "%s  %s\n", playlist.Title, playlist.ID)
	if playlist.Description != "" {
		_, _ = fmt.Fprintln(out, playlist.Description)
	}
	_, _ = fmt.Fprintf(out, "%s, %d with a tracklist, %d unavailable, %s\n\n",
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
	table(out, []string{"#", "VIDEO", "TITLE", "CHANNEL", "LENGTH", "TRACKS", "STATE"}, rows)
}

// state is what is worth saying about a video beyond its own fields: that
// YouTube will not serve it, or that its tracklist has not been read.
func state(video api.VideoSummary) string {
	switch {
	case video.IsUnavailable:
		return "unavailable"
	case video.EnrichedTs == nil:
		return "unread"
	}
	return ""
}
