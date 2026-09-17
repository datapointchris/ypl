// Package ytdlp reads videos from YouTube through the yt-dlp binary, signed in
// as nobody. The Data API reports neither a video's chapters nor its comments,
// and yt-dlp reads both from the watch page.
//
// Each read is one yt-dlp process, bounded by the caller's context. YouTube
// throttles an address that reads too much too fast, so a caller paces its
// reads and stops at ErrRateLimited.
package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/datapointchris/ypl/api/tracklist"
)

// ErrRateLimited is YouTube refusing reads from this address for now, with its
// bot check or its rate limit. Reading on is what turns a pause into a block.
var ErrRateLimited = errors.New("YouTube is refusing reads from this address for now")

// ErrUnreadable is YouTube answering that no read by a signed-out session will
// ever return the video: private, removed, members-only, blocked in this
// country, or age-restricted.
var ErrUnreadable = errors.New("YouTube will not let a signed-out session read this video")

// MaxComments is how many of a video's top comments a read returns. A
// tracklist someone posted is among the top few when there is one.
const MaxComments = 20

// waitDelay bounds how long a read waits for yt-dlp's output to close once its
// context ends and the process is killed. A process yt-dlp started can hold the
// output open after yt-dlp itself is gone.
const waitDelay = 5 * time.Second

// Video is what a full read of a video reports that a playlist read through
// the Data API does not. DurationSeconds is 0 for a video with no duration, such
// as a live stream, and UploadDate an ISO date, empty when YouTube reports none.
// Comments are its top comments, most relevant first.
type Video struct {
	ID              string
	DurationSeconds int64
	Description     string
	UploadDate      string
	Chapters        []tracklist.Chapter
	Comments        []string
}

// Reader reads videos with the yt-dlp binary at path.
type Reader struct {
	path string
}

// NewReader is a Reader of the yt-dlp binary name, found on PATH when it holds
// no separator.
func NewReader(name string) (*Reader, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("find yt-dlp as %q: %w", name, err)
	}
	return &Reader{path: path}, nil
}

// videoID is the shape of a YouTube video id.
var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// Video reads the video id: its details, chapters and top comments. The error
// wraps ErrRateLimited or ErrUnreadable when YouTube's answer is one of those,
// and ctx's error when ctx ended first.
func (r *Reader) Video(ctx context.Context, id string) (Video, error) {
	if !videoID.MatchString(id) {
		return Video{}, fmt.Errorf("read video %q: not a YouTube video id", id)
	}
	cmd := exec.CommandContext(ctx, r.path, arguments(id)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Video{}, fmt.Errorf("read video %s: %w", id, ctxErr)
	}
	if err != nil {
		return Video{}, fmt.Errorf("read video %s: %w", id, refusal(stderr.String(), err))
	}
	video, err := decode(stdout.Bytes())
	if err != nil {
		return Video{}, fmt.Errorf("read video %s: %w", id, err)
	}
	if video.ID != id {
		return Video{}, fmt.Errorf("read video %s: yt-dlp answered with video %q", id, video.ID)
	}
	return video, nil
}

// arguments is what a read of the video id passes yt-dlp. It reads no config
// file, so the host's own yt-dlp settings cannot change what a read does, and it
// waits a second between the requests one read makes.
func arguments(id string) []string {
	return []string{
		"--ignore-config",
		"--skip-download",
		"--no-playlist",
		"--sleep-requests", "1",
		"--write-comments",
		"--extractor-args", fmt.Sprintf("youtube:max_comments=%d,%d,0,0;comment_sort=top", MaxComments, MaxComments),
		"--dump-json",
		"--",
		"https://www.youtube.com/watch?v=" + id,
	}
}

// rateLimitMarkers are what yt-dlp's error says when YouTube refuses the
// address rather than the video. YouTube words a rate limit as "Video
// unavailable. This content isn't available, try again later", so these are
// looked for before unreadableMarkers.
var rateLimitMarkers = []string{
	"rate-limited by youtube",
	"this content isn't available, try again later",
	"confirm you're not a bot",
	"http error 429",
	"too many requests",
}

// unreadableMarkers are what yt-dlp's error says when the video itself is
// closed to a signed-out read. A bare "Video unavailable" is not one, since
// YouTube's rate limit opens with it too.
var unreadableMarkers = []string{
	"this video is private",
	"private video",
	"has been removed",
	"was removed",
	"was blocked",
	"video is no longer available",
	"this video is not available",
	"members-only",
	"members only",
	"not made this video available in your country",
	"not available in your country",
	"sign in to confirm your age",
	"inappropriate for some users",
}

// Unreadable is whether yt-dlp's error message says YouTube will never let a
// signed-out session read the video, and not that it refuses reads from this
// address for now.
func Unreadable(message string) bool {
	said := normalized(message)
	return !containsAny(said, rateLimitMarkers) && containsAny(said, unreadableMarkers)
}

// refusal is yt-dlp's failure as an error naming its last error line, wrapping
// ErrRateLimited or ErrUnreadable when the error lines carry one of their
// markers. Only error lines are read, since a warning about a request yt-dlp
// retried says nothing about how the read ended.
func refusal(stderr string, exit error) error {
	var errorLines []string
	for line := range strings.Lines(stderr) {
		if strings.HasPrefix(line, "ERROR:") {
			errorLines = append(errorLines, strings.TrimSpace(line))
		}
	}
	if len(errorLines) == 0 {
		return fmt.Errorf("yt-dlp failed with no error line: %w", exit)
	}
	message := errorLines[len(errorLines)-1]
	said := strings.Join(errorLines, "\n")
	switch {
	case containsAny(normalized(said), rateLimitMarkers):
		return fmt.Errorf("%w: %s", ErrRateLimited, message)
	case Unreadable(said):
		return fmt.Errorf("%w: %s", ErrUnreadable, message)
	default:
		return fmt.Errorf("yt-dlp: %s", message)
	}
}

// normalized is message lowercased with its curly apostrophes straight, as the
// markers are written.
func normalized(message string) string {
	return strings.ToLower(strings.ReplaceAll(message, "’", "'"))
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// info is the part of yt-dlp's JSON for a video that a read keeps.
type info struct {
	ID          string   `json:"id"`
	Duration    *float64 `json:"duration"`
	Description string   `json:"description"`
	UploadDate  string   `json:"upload_date"`
	Chapters    []struct {
		StartTime float64 `json:"start_time"`
		EndTime   float64 `json:"end_time"`
		Title     string  `json:"title"`
	} `json:"chapters"`
	Comments []struct {
		Text string `json:"text"`
	} `json:"comments"`
}

// decode is the video yt-dlp's JSON describes.
func decode(output []byte) (Video, error) {
	var read info
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&read); err != nil {
		return Video{}, fmt.Errorf("decode yt-dlp's answer: %w", err)
	}
	video := Video{ID: read.ID, Description: read.Description}
	if read.Duration != nil {
		video.DurationSeconds = int64(*read.Duration)
	}
	if read.UploadDate != "" {
		uploaded, err := time.Parse("20060102", read.UploadDate)
		if err != nil {
			return Video{}, fmt.Errorf("decode yt-dlp's upload date %q: %w", read.UploadDate, err)
		}
		video.UploadDate = uploaded.Format(time.DateOnly)
	}
	for _, chapter := range read.Chapters {
		video.Chapters = append(video.Chapters, tracklist.Chapter{
			StartSeconds: int64(chapter.StartTime),
			EndSeconds:   int64(chapter.EndTime),
			Title:        chapter.Title,
		})
	}
	for _, comment := range read.Comments {
		video.Comments = append(video.Comments, comment.Text)
	}
	return video, nil
}
