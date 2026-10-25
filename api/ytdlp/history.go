package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrSignedOut is YouTube answering a watch-history read as a signed-out
// session: the cookie jar expired, was revoked, or was opened in a browser that
// rotated it.
var ErrSignedOut = errors.New("YouTube answered the watch history signed out")

// HistoryEntry is one video of the watch history, newest first. The history
// says what was started and in what order, never when.
//
// ResumePercent is how far into the video YouTube's resume point sits, the
// whole percentage its history page draws as a progress bar, and nil where the
// page carried none.
type HistoryEntry struct {
	ID            string
	Title         string
	ResumePercent *int
}

// signedOutMarkers are what yt-dlp's error says when the history needs a
// session the jar does not hold.
var signedOutMarkers = []string{
	"login details are needed",
	"use --cookies",
	"not logged in",
	"sign in to view",
}

// History reads the newest entries of the watch history of the account whose
// cookies jar holds, in Netscape format, and returns them with the jar yt-dlp
// wrote back, which carries whatever cookies YouTube rotated during the read.
// The error wraps ErrSignedOut when YouTube answered signed out, ErrRateLimited
// when it refused the address, and ctx's error when ctx ended first.
//
// The jar is written to a file only this process can read, in a directory
// removed when the read ends, since yt-dlp takes cookies only from a file.
func (r *Reader) History(ctx context.Context, jar string, entries int) ([]HistoryEntry, string, error) {
	dir, err := os.MkdirTemp("", "ypl-history-")
	if err != nil {
		return nil, "", fmt.Errorf("read the watch history: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(jar), 0o600); err != nil {
		return nil, "", fmt.Errorf("read the watch history: %w", err)
	}
	cmd := exec.CommandContext(ctx, r.path, historyArguments(path, entries)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, "", fmt.Errorf("read the watch history: %w", ctxErr)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the watch history: %w", historyRefusal(stderr.String(), err))
	}
	read, err := decodeHistory(stdout.Bytes())
	if err != nil {
		return nil, "", fmt.Errorf("read the watch history: %w", err)
	}
	percents, err := resumePercents(dir)
	if err != nil {
		slog.Warn("the watch history was read without its resume points", "err", err)
	}
	for i := range read {
		if percent, ok := percents[read[i].ID]; ok {
			read[i].ResumePercent = &percent
		}
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read back the cookie jar: %w", err)
	}
	return read, string(written), nil
}

// historyArguments is what a read of the watch history passes yt-dlp: no
// config file, no plugin, no cache, and only the list of entries rather than
// each video's page. --write-pages leaves the history page it read in the
// working directory, which is where the resume points are.
func historyArguments(cookies string, entries int) []string {
	return []string{
		"--ignore-config",
		"--no-plugin-dirs",
		"--no-cache-dir",
		"--cookies", cookies,
		"--write-pages",
		"--flat-playlist",
		"--playlist-end", strconv.Itoa(entries),
		"--dump-single-json",
		"--",
		":ythis",
	}
}

// historyRefusal is refusal with a signed-out answer told apart, after the
// rate limit, whose bot check also asks to sign in.
func historyRefusal(stderr string, exit error) error {
	err := refusal(stderr, exit)
	if errors.Is(err, ErrRateLimited) {
		return err
	}
	if containsAny(normalized(stderr), signedOutMarkers) {
		return fmt.Errorf("%w: %s", ErrSignedOut, strings.TrimPrefix(err.Error(), "yt-dlp: "))
	}
	return err
}

func decodeHistory(output []byte) ([]HistoryEntry, error) {
	var read struct {
		Entries []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&read); err != nil {
		return nil, fmt.Errorf("decode yt-dlp's answer: %w", err)
	}
	entries := make([]HistoryEntry, 0, len(read.Entries))
	for _, entry := range read.Entries {
		if videoID.MatchString(entry.ID) {
			entries = append(entries, HistoryEntry{ID: entry.ID, Title: entry.Title})
		}
	}
	return entries, nil
}

// resumePercents is each video's resume point on the history pages yt-dlp
// wrote into dir, by video id. The error says no page could be read, which the
// history answers through without them; a page with no progress bar on it is
// no error.
func resumePercents(dir string) (map[string]int, error) {
	percents := map[string]int{}
	pages, err := filepath.Glob(filepath.Join(dir, "*.dump"))
	if err != nil || len(pages) == 0 {
		return percents, fmt.Errorf("yt-dlp left no history page in %s", dir)
	}
	read := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			continue
		}
		data, ok := initialData(body)
		if !ok {
			continue
		}
		read++
		lockupPercents(data, percents)
	}
	if read == 0 {
		return percents, errors.New("no history page held ytInitialData that decodes")
	}
	return percents, nil
}

// initialDataMarker is each way a page assigns ytInitialData, as yt-dlp's own
// _YT_INITIAL_DATA_RE accepts them.
var initialDataMarker = regexp.MustCompile(`ytInitialData"?\]?\s*=\s*`)

// initialData is the ytInitialData object a YouTube page embeds.
func initialData(page []byte) (any, bool) {
	at := initialDataMarker.FindIndex(page)
	if at == nil {
		return nil, false
	}
	var data any
	if err := json.NewDecoder(bytes.NewReader(page[at[1]:])).Decode(&data); err != nil {
		return nil, false
	}
	return data, true
}

// lockupPercents adds to percents every video lockup under node with the
// progress bar's start percent drawn on it.
func lockupPercents(node any, percents map[string]int) {
	switch value := node.(type) {
	case map[string]any:
		if lockup, ok := value["lockupViewModel"].(map[string]any); ok {
			if id, ok := lockup["contentId"].(string); ok && videoID.MatchString(id) {
				if percent, ok := progressPercent(lockup); ok {
					if _, seen := percents[id]; !seen {
						percents[id] = percent
					}
				}
			}
			return
		}
		for _, child := range value {
			lockupPercents(child, percents)
		}
	case []any:
		for _, child := range value {
			lockupPercents(child, percents)
		}
	}
}

// progressPercent is the first progress bar's start percent under node.
func progressPercent(node any) (int, bool) {
	switch value := node.(type) {
	case map[string]any:
		if bar, ok := value["thumbnailOverlayProgressBarViewModel"].(map[string]any); ok {
			percent, ok := bar["startPercent"].(float64)
			return int(percent), ok
		}
		for _, child := range value {
			if percent, ok := progressPercent(child); ok {
				return percent, true
			}
		}
	case []any:
		for _, child := range value {
			if percent, ok := progressPercent(child); ok {
				return percent, true
			}
		}
	}
	return 0, false
}
