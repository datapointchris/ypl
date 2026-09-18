package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// wholeName is how a verb that changes a playlist is told which one. The server
// resolves a change exactly, so part of a title that a read would take names
// nothing here.
const wholeName = "Name the playlist by its whole title, its slug from Tab, or its id;\n" +
	"part of a title is not enough."

func (a *app) playlistsCreateCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "create <title>",
		GroupID: groupChanging,
		Short:   "Create an empty private playlist",
		Long:    "`ypl playlists edit` puts videos in it.",
		Example: "  ypl playlists create 'Sunday Morning'         a new empty playlist\n" +
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
		GroupID: groupChanging,
		Short:   "Rename a playlist on YouTube",
		Long:    wholeName,
		Example: "  ypl playlists rename 'Sunday Morning' 'Sunday Mornings'  retitle it\n" +
			"  ypl playlists rename PLabc123 'Sunday Mornings'          by its id",
		Args:              usageArgs(cobra.ExactArgs(2)),
		ValidArgsFunction: onlyFirst(a.completePlaylists),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			renamed, err := client.RenamePlaylist(cmd.Context(), api.Reference(args[0]), api.PlaylistTitle(args[1]))
			if err != nil {
				return reported(namingPlaylists(cmd.Context(), client, err))
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
		GroupID: groupChanging,
		Short:   "Delete a playlist from YouTube",
		Long:    "It cannot be undone. It asks first; --yes answers for it.\n" + wholeName,
		Example: "  ypl playlists delete 'Sunday Morning'        ask, then delete it\n" +
			"  ypl playlists delete 'Sunday Morning' --yes  delete it without asking",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: onlyFirst(a.completePlaylists),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := api.Reference(args[0])
			// Checked before anything is read, so a caller who could never have
			// answered is told what they left out rather than told that a write
			// did not happen.
			if !yes {
				if err := a.confirmable(cmd); err != nil {
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
					return reported(namingPlaylists(cmd.Context(), client, err))
				}
				// Read second, so the question names what is about to go rather
				// than repeating back what was typed.
				playlist, err := client.GetPlaylist(cmd.Context(), name)
				if err != nil {
					return reported(namingPlaylists(cmd.Context(), client, err))
				}
				deleting = playlist.Title
				question := fmt.Sprintf("Delete %s (%s) from YouTube?", playlist.Title, count(playlist.ItemCount, "video"))
				approved, err := a.confirm(cmd, question)
				if err != nil {
					return err
				}
				if !approved {
					nothing(cmd, "Nothing was deleted.")
					return exitCode(1)
				}
			}
			if err := client.DeletePlaylist(cmd.Context(), name); err != nil {
				return reported(namingPlaylists(cmd.Context(), client, err))
			}
			nothing(cmd, fmt.Sprintf("Deleted %s.", deleting))
			return nil
		},
	}
	addYes(cmd, &yes, "delete it")
	addNoInput(cmd, "refuse rather than ask, unless --yes answers")
	return cmd
}
