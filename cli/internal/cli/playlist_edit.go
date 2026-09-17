package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/editbuffer"
)

// edited is what an edit did, and what the playlist holds afterwards.
type edited struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	VideoCount int      `json:"video_count"`
	Added      []string `json:"added"`
	Removed    []string `json:"removed"`
	Reordered  bool     `json:"reordered"`
}

// changed reports whether the buffer asked for anything at all.
func (e edited) changed() bool { return len(e.Added) > 0 || len(e.Removed) > 0 || e.Reordered }

func (a *app) playlistsEditCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "edit <playlist>",
		GroupID: groupPlaylistWriting,
		Short:   "Rearrange a playlist in your editor",
		Long: "Opens one line per video — the id first, then the title — in $VISUAL or\n" +
			"$EDITOR. Move lines to reorder, delete a line to remove that video, paste a\n" +
			"URL or an id on its own line to add one. Save to apply, or save an empty\n" +
			"buffer to abort.\n" +
			"\n" +
			"Modeled on `git rebase -i`, because rearranging a list is something your\n" +
			"editor is already better at than any command could be. Reads the buffer from\n" +
			"stdin instead when something is piped in.\n" +
			"\n" +
			"This changes the order the server holds. The next sync run pushes it to\n" +
			"YouTube, so `ypl status` is where it shows up as sent.",
		Example: "  ypl playlists edit 'Sunday Morning'          rearrange it in your editor\n" +
			"  ypl playlists edit 'Sunday Morning' < order  apply a buffer written elsewhere",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			// The playlist read is for the titles the buffer shows. The order
			// read is what the buffer is made of and what an edit of it names,
			// so it is read second and it is the one that decides the lines.
			playlist, err := client.GetPlaylist(cmd.Context(), name)
			if err != nil {
				return reported(err)
			}
			order, err := client.PlaylistOrder(cmd.Context(), name)
			if err != nil {
				return reported(err)
			}

			buffer := editbuffer.Render(playlist.Title, rows(playlist, order.VideoIDs))
			text, err := readBuffer(cmd, buffer)
			if err != nil {
				return err
			}
			videoIDs, err := editbuffer.Parse(text, held(order.VideoIDs))
			if err != nil {
				return goclikit.UsageError(fmt.Errorf("%w, and nothing was changed", err))
			}

			// The lists are empty rather than absent at every size, so a caller
			// filtering the JSON writes one filter and no null guard.
			result := edited{ID: playlist.ID, Title: playlist.Title, VideoCount: len(order.VideoIDs), Added: []string{}, Removed: []string{}}
			// An empty buffer aborts rather than emptying the playlist, which is
			// what `git rebase -i` does and what somebody who deleted every line
			// by accident needs it to do.
			if len(videoIDs) > 0 && !slices.Equal(videoIDs, order.VideoIDs) {
				replaced, err := client.ReplacePlaylistOrder(cmd.Context(), name, order.Revision, videoIDs)
				if err != nil {
					return reported(errors.Join(err, keptAt(text)))
				}
				result = difference(playlist, order.VideoIDs, replaced.VideoIDs)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), result)
			}
			// A playlist that was already empty is not a buffer somebody
			// emptied, and telling them an empty buffer aborts would name a
			// decision they never made.
			reportEdit(cmd, result, len(videoIDs) == 0 && len(order.VideoIDs) > 0)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "what the edit changed")
	return cmd
}

// readBuffer is the text an edit works from: what came back from the editor, or
// what a caller piped in.
//
// A piped buffer is taken whether or not --no-input was passed, since a pipe is
// not a prompt. What --no-input forbids is taking the terminal, and there is
// nothing else to read from there, so it refuses rather than reading a terminal
// nobody is typing into.
func readBuffer(cmd *cobra.Command, buffer string) (string, error) {
	if !terminalIn(cmd) {
		piped, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read the buffer from stdin: %w", err)
		}
		return string(piped), nil
	}
	if !interactive(cmd) {
		return "", goclikit.UsageError(errors.New("refusing to open an editor with --no-input; pipe a buffer in instead"))
	}
	text, changedIt, err := editbuffer.Open(buffer)
	if err != nil {
		return "", err
	}
	// Opening the editor and closing it again is a different outcome from
	// rearranging the lines back to where they were, and only the first is
	// certain to have asked for nothing.
	if !changedIt {
		return buffer, nil
	}
	return text, nil
}

// rows is one line of the buffer per slot of the order, with the title of the
// video that slot holds.
//
// The two reads are of one playlist a moment apart, so a sync between them can
// leave a slot whose video the playlist read did not carry. That slot still gets
// its line, with its id and no title: the id is what an edit is made of, and a
// missing title costs a line its label rather than costing the buffer a video.
func rows(playlist api.Playlist, videoIDs []string) []editbuffer.Row {
	known := map[string]api.VideoSummary{}
	for _, item := range playlist.Items {
		known[item.Video.ID] = item.Video
	}
	made := make([]editbuffer.Row, len(videoIDs))
	for i, videoID := range videoIDs {
		video := known[videoID]
		made[i] = editbuffer.Row{VideoID: videoID, Label: label(video), Length: clock(video.DurationSeconds)}
	}
	return made
}

// label is what a video is called on its line: the channel and the title, and
// whichever one of them there is when the other is empty.
func label(video api.VideoSummary) string {
	switch {
	case video.ChannelTitle == "":
		return video.Title
	case video.Title == "":
		return video.ChannelTitle
	}
	return video.ChannelTitle + " - " + video.Title
}

// held is the ids the buffer was rendered from, which Parse takes as given
// however odd they look.
func held(videoIDs []string) map[string]bool {
	known := make(map[string]bool, len(videoIDs))
	for _, videoID := range videoIDs {
		known[videoID] = true
	}
	return known
}

// difference is what the edit did: the videos it added, the videos it removed,
// and whether what was in both changed places.
//
// Reordering is judged on what survived. Removing a video shifts everything
// below it and adding one pushes everything after it along, so comparing the
// two orders whole would report every addition as a reordering as well.
func difference(playlist api.Playlist, before, after []string) edited {
	added := missingFrom(before, after)
	removed := missingFrom(after, before)
	return edited{
		ID:         playlist.ID,
		Title:      playlist.Title,
		VideoCount: len(after),
		Added:      added,
		Removed:    removed,
		Reordered:  !slices.Equal(without(before, removed), without(after, added)),
	}
}

// missingFrom is every entry of want that from does not have, counting repeats:
// a video in three slots where it was in one is added twice.
func missingFrom(from, want []string) []string {
	left := map[string]int{}
	for _, videoID := range from {
		left[videoID]++
	}
	found := []string{}
	for _, videoID := range want {
		if left[videoID] > 0 {
			left[videoID]--
			continue
		}
		found = append(found, videoID)
	}
	return found
}

// without is videoIDs with one slot dropped for each entry of drop.
func without(videoIDs, drop []string) []string {
	left := map[string]int{}
	for _, videoID := range drop {
		left[videoID]++
	}
	kept := make([]string, 0, len(videoIDs))
	for _, videoID := range videoIDs {
		if left[videoID] > 0 {
			left[videoID]--
			continue
		}
		kept = append(kept, videoID)
	}
	return kept
}

// reportEdit writes what the edit did, for a person. An edit that asked for
// nothing says which of the two ways it asked for nothing, since an empty buffer
// is a deliberate abort and an untouched one is not.
func reportEdit(cmd *cobra.Command, result edited, emptied bool) {
	if !result.changed() {
		if emptied {
			nothing(cmd, fmt.Sprintf("%s unchanged — an empty buffer aborts.", result.Title))
			return
		}
		nothing(cmd, fmt.Sprintf("%s unchanged.", result.Title))
		return
	}
	said := []string{}
	if len(result.Added) > 0 {
		said = append(said, count(int64(len(result.Added)), "video")+" added")
	}
	if len(result.Removed) > 0 {
		said = append(said, count(int64(len(result.Removed)), "video")+" removed")
	}
	if result.Reordered {
		said = append(said, "reordered")
	}
	// The total is its own sentence. Joined onto the changes it reads as one
	// more of them, and "2 videos removed, 1 video" names three things that
	// happened, one of which did not.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s — %s. It now holds %s.\n",
		result.Title, strings.Join(said, ", "), count(int64(result.VideoCount), "video"))
	nothing(cmd, "The next sync run pushes it to YouTube. `ypl status` says when that was.")
}

// keptAt writes the edited buffer somewhere it can be read back from, and is the
// sentence saying where. A refusal arrives after the editor has closed, so
// without this the rearranging is gone and the only way back is to do it again.
func keptAt(text string) error {
	file, err := os.CreateTemp("", "ypl-edit-*.ypl")
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		return nil
	}
	return fmt.Errorf("the buffer is kept at %s, and `ypl playlists edit <playlist> < %s` applies it", file.Name(), file.Name())
}
