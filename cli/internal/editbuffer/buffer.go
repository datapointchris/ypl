// Package editbuffer is the text a playlist becomes while it is being edited.
//
// Modeled on `git rebase -i`, and for the same reason: rearranging a list is
// something text editors are already extremely good at, and any bespoke
// interface for it starts out worse than the one already open. One line per
// video, the id first so it can be read back, the title after it so the line
// means something. Move lines to reorder, delete lines to remove, save to
// apply.
//
// The id is on the line but never typed. That is the whole point — identifying
// a video by pasting eleven characters is what made editing a playlist while it
// was playing intolerable, and no amount of extra commands fixes it. The editor
// already knows how to move a line.
//
// The format is pure — text in, ids out — so it is tested by writing a string.
// Open is the one effectful thing here, and it lives beside the format rather
// than in the command layer because the two are one idea.
package editbuffer

import (
	"fmt"
	"net/url"
	"strings"
)

// comment begins a line the buffer ignores.
const comment = "#"

// instructions are what the buffer says about itself, above the videos.
const instructions = `# Reorder these lines to reorder the playlist.
# Delete a line to remove that video from it.
# Add a line with a URL or id to add one.
#
# Lines starting with # are ignored. Save an empty buffer to abort.`

// idLength is how many characters a YouTube video id has, and idAlphabet what
// they are drawn from.
const idLength = 11

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// watchHosts serve a video at /watch?v=; shortHosts serve one at /<id>.
var watchHosts = map[string]bool{
	"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true, "music.youtube.com": true,
}

var shortHosts = map[string]bool{"youtu.be": true}

// pathPrefixes are the paths that carry an id as their next segment.
var pathPrefixes = []string{"/shorts/", "/embed/", "/v/", "/live/"}

// idColumn and labelColumn are what the two padded columns are widened to. The
// id column is fixed because YouTube ids are, and the columns are padded rather
// than tab-separated so the titles line up in any editor.
const (
	idColumn    = 12
	labelColumn = 60
)

// Row is one video as the buffer shows it. Label and Length are already written
// out, so this package formats no value it would have to agree with the tables
// about.
type Row struct {
	VideoID string
	Label   string
	Length  string
}

// LineError is a line in an edited buffer that is not something that can be
// applied. It carries the line number, because the answer to "which one" is the
// whole difference between fixing it and reopening the editor to hunt.
type LineError struct {
	Number int
	Line   string
	Reason string
}

func (e *LineError) Error() string {
	return fmt.Sprintf("line %d: %s: %q", e.Number, e.Reason, strings.TrimSpace(e.Line))
}

// Render is the buffer for one playlist, headed by what it holds.
func Render(name string, rows []Row) string {
	lines := []string{
		strings.TrimRight(fmt.Sprintf("%s %s — %d videos", comment, name, len(rows)), " "),
		comment,
		instructions,
		comment,
	}
	for _, row := range rows {
		line := fmt.Sprintf("%-*s %-*s %s", idColumn, row.VideoID, labelColumn, row.Label, row.Length)
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return strings.Join(lines, "\n") + "\n"
}

// Parse is the video ids an edited buffer asks for, in the order it puts them.
//
// known is what the buffer was rendered from, and a token in it is taken as
// given however odd it looks. Without that, an id that does not match the
// eleven-character rule — a hand-edited playlist, a form YouTube has not used
// in fifteen years — would be written into the buffer and then rejected when
// the same buffer was read back, which is a tool refusing to accept its own
// output. Anything else has to look like an id, so that a line of prose is
// still caught rather than filed as a video.
//
// Duplicates are allowed through: a playlist may legitimately hold the same mix
// twice, and the buffer is the person saying what they want.
func Parse(text string, known map[string]bool) ([]string, error) {
	videoIDs := []string{}
	for i, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, comment) {
			continue
		}
		token := strings.Fields(stripped)[0]
		videoID := token
		if !known[token] {
			videoID = VideoID(token)
		}
		if videoID == "" {
			return nil, &LineError{Number: i + 1, Line: line, Reason: "does not start with a video id or URL"}
		}
		videoIDs = append(videoIDs, videoID)
	}
	return videoIDs, nil
}

// VideoID is the video a token names: a bare id, or the id inside a YouTube
// URL. It is "" for anything else.
//
// A URL is read structurally rather than matched, so one carrying extra query
// parameters — `&list=`, `&t=`, the tracking ones YouTube's share button
// appends — yields the same id as the bare link. What a URL names is taken as
// given, since `v=` says what it is; only a bare token has to look like an id,
// which is what keeps a line of prose from being filed as a video.
func VideoID(token string) string {
	candidate := strings.TrimSpace(token)
	switch {
	case candidate == "":
		return ""
	case !strings.Contains(candidate, "/"):
		if isVideoID(candidate) {
			return candidate
		}
		return ""
	}
	// A URL written without a scheme parses as a path, and then the host this
	// reads is empty. The `//` makes it an authority either way.
	if !strings.Contains(candidate, "//") {
		candidate = "//" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	if shortHosts[host] {
		return firstSegment(parsed.Path)
	}
	if !watchHosts[host] {
		return ""
	}
	if named := parsed.Query()["v"]; len(named) > 0 {
		return named[0]
	}
	for _, prefix := range pathPrefixes {
		if strings.HasPrefix(parsed.Path, prefix) {
			return firstSegment(strings.TrimPrefix(parsed.Path, prefix))
		}
	}
	return ""
}

// isVideoID reports whether text is an id as YouTube writes one.
func isVideoID(text string) bool {
	if len(text) != idLength {
		return false
	}
	return strings.IndexFunc(text, func(r rune) bool {
		return !strings.ContainsRune(idAlphabet, r)
	}) < 0
}

// firstSegment is the first path segment of path, and "" where there is none.
func firstSegment(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return segment
}
