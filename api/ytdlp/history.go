package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrSignedOut is YouTube answering a watch-history read as a signed-out
// session: the cookie jar expired, was revoked, or was opened in a browser that
// rotated it.
var ErrSignedOut = errors.New("YouTube answered the watch history signed out")

// HistoryEntry is one video of the watch history, newest first. The history
// says what was started and in what order, never when.
type HistoryEntry struct {
	ID    string
	Title string
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
	// A session YouTube no longer honors can come back as a page with no
	// history on it rather than as an error, and an account in use always has
	// some.
	if len(read) == 0 {
		return nil, "", fmt.Errorf("read the watch history: %w: it came back empty", ErrSignedOut)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read back the cookie jar: %w", err)
	}
	return read, string(written), nil
}

// historyArguments is what a read of the watch history passes yt-dlp: no
// config file, no plugin, no cache, and only the list of entries rather than
// each video's page.
func historyArguments(cookies string, entries int) []string {
	return []string{
		"--ignore-config",
		"--no-plugin-dirs",
		"--no-cache-dir",
		"--cookies", cookies,
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
