package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

func (a *app) playlistsCreateCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "create <title>",
		GroupID: groupPlaylistWriting,
		Short:   "Make a new playlist on the channel",
		Long: "Creates the playlist on YouTube, private, and stores it. It is made in this\n" +
			"request rather than by the next sync run, so the id it comes back with is\n" +
			"YouTube's own.\n" +
			"\n" +
			"It is empty. `ypl playlists edit` is what puts videos in one.",
		Example: "  ypl playlists create 'Sunday Morning'         a new empty playlist, private on YouTube\n" +
			"  ypl playlists create 'Sunday Morning' --json  the same, for a script",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			created, err := client.CreatePlaylist(cmd.Context(), api.PlaylistTitle(args[0]))
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), created)
			}
			printPlaylists(cmd.OutOrStdout(), []api.PlaylistSummary{created})
			nothing(cmd, "It is empty and private. `ypl playlists edit` opens it in your editor.")
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the playlist")
	return cmd
}

func (a *app) playlistsRenameCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "rename <playlist> <title>",
		GroupID: groupPlaylistWriting,
		Short:   "Retitle a playlist, on YouTube and here",
		Long: "Renames it on YouTube in this request. The playlist is named by its id, its\n" +
			"whole title, or that title slugged — part of a title does not reach a rename,\n" +
			"because a fragment matching one playlist matches it unambiguously.",
		Example: "  ypl playlists rename 'Sunday Morning' 'Sunday Mornings'  retitle it\n" +
			"  ypl playlists rename PLabc123 'Sunday Mornings'          name it by its id instead",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			renamed, err := client.RenamePlaylist(cmd.Context(), api.Reference(args[0]), api.PlaylistTitle(args[1]))
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), renamed)
			}
			printPlaylists(cmd.OutOrStdout(), []api.PlaylistSummary{renamed})
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the playlist")
	return cmd
}

func (a *app) playlistsDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete <playlist>",
		GroupID: groupPlaylistWriting,
		Short:   "Delete a playlist from the channel",
		Long: "Deletes it on YouTube in this request, and from the server with everything it\n" +
			"held. Nothing here can put it back: the videos are still in the library, and\n" +
			"the playlist that gathered them is gone.\n" +
			"\n" +
			"It asks first, and needs --yes where there is nobody to ask. The playlist is\n" +
			"named by its id, its whole title, or that title slugged — part of a title does\n" +
			"not reach a delete.",
		Example: "  ypl playlists delete 'Sunday Morning'        ask, then delete it\n" +
			"  ypl playlists delete 'Sunday Morning' --yes  delete it without asking",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := api.Reference(args[0])
			// Checked before anything is read, so a caller who could never have
			// answered is told what they left out rather than told that a write
			// did not happen.
			if !yes {
				if err := confirmable(cmd); err != nil {
					return err
				}
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			deleting := args[0]
			if !yes {
				// The order is read for nothing it holds. It is the one read the
				// server resolves as narrowly as it resolves this delete, so a
				// reference the delete will refuse is refused here instead of
				// after the answer. Asking first and refusing after is how
				// somebody comes to approve destroying a playlist on a command
				// that could never have done it.
				if _, err := client.PlaylistItems(cmd.Context(), name); err != nil {
					return reported(err)
				}
				// Read second, so the question names what is about to go rather
				// than repeating back what was typed.
				playlist, err := client.GetPlaylist(cmd.Context(), name)
				if err != nil {
					return reported(err)
				}
				deleting = playlist.Title
				question := fmt.Sprintf("Delete %s (%s) from YouTube?", playlist.Title, count(playlist.ItemCount, "video"))
				approved, err := confirm(cmd, question)
				if err != nil {
					return err
				}
				if !approved {
					nothing(cmd, "Nothing was deleted.")
					return exitCode(1)
				}
			}
			if err := client.DeletePlaylist(cmd.Context(), name); err != nil {
				return reported(err)
			}
			nothing(cmd, fmt.Sprintf("Deleted %s.", deleting))
			return nil
		},
	}
	addYes(cmd, &yes, "delete it")
	return cmd
}
