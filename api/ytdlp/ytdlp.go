// Package ytdlp reads videos from YouTube through the yt-dlp binary, signed in
// as nobody except for the watch history a person opens. The Data API reports
// neither a video's chapters, its comments nor its captions, and yt-dlp reads
// each from the watch page.
//
// Every read goes through one Door, which runs one yt-dlp process at a time,
// paced apart, and pauses every caller once YouTube refuses the address. Each
// read is one turn at the door, bounded by the caller's context and the read's
// own limit.
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
// Comments are its top comments, most relevant first, and CommentsCapped says
// the video has more than MaxComments of them, so what the read did not return
// is the rest rather than nothing.
//
// The cap is counted here rather than taken from yt-dlp's comment_count, which
// its comment post-extractor overwrites with how many it downloaded. That field
// reports the cap as the total.
type Video struct {
	ID              string
	DurationSeconds int64
	Description     string
	UploadDate      string
	Chapters        []tracklist.Chapter
	Comments        []string
	CommentsCapped  bool
}

// Binary is the yt-dlp binary the door runs.
type Binary struct {
	path string
}

// Find is the yt-dlp binary name, found on PATH when it holds no separator.
func Find(name string) (Binary, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return Binary{}, fmt.Errorf("find yt-dlp as %q: %w", name, err)
	}
	return Binary{path: path}, nil
}

// run runs the binary with args in dir, or the server's own directory where dir
// is empty, and returns what it wrote to stdout. A failure is refused's reading
// of what it wrote to stderr, and ctx's error once ctx ended.
func (b Binary) run(ctx context.Context, dir string, args []string, refused func(stderr string, exit error) error) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.path, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, refused(stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

// videoID is the shape of a YouTube video id.
var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// videoLimit bounds one read of a video's details and comments.
const videoLimit = 2 * time.Minute

// Video reads the video id for enrichment: its details, chapters and top
// comments. The error wraps ErrRateLimited or ErrUnreadable when YouTube's
// answer is one of those, ErrPaused while the door is paused, and ctx's error
// when ctx ended first.
func (d *Door) Video(ctx context.Context, id string) (Video, error) {
	if !videoID.MatchString(id) {
		return Video{}, fmt.Errorf("read video %q: not a YouTube video id", id)
	}
	video, err := through(ctx, d, Enrichment, videoLimit, func(ctx context.Context) (Video, error) {
		output, err := d.binary.run(ctx, "", arguments(id), refusal)
		if err != nil {
			return Video{}, err
		}
		video, err := decode(output)
		if err == nil && video.ID != id {
			err = fmt.Errorf("yt-dlp answered with video %q", video.ID)
		}
		return video, err
	})
	if err != nil {
		return Video{}, fmt.Errorf("read video %s: %w", id, err)
	}
	return video, nil
}

// paced is what every read passes yt-dlp ahead of what it reads. It reads no
// config file and loads no plugin, which are separate settings and either of
// which would let the host change what a read does. The sleeps pace the
// requests inside one read, which the door's gap does not reach: 2 seconds
// between requests, 5 before each subtitle, and 8 to 16 before each download.
func paced() []string {
	return []string{
		"--ignore-config",
		"--no-plugin-dirs",
		"--sleep-requests", "2",
		"--sleep-subtitles", "5",
		"--sleep-interval", "8",
		"--max-sleep-interval", "16",
	}
}

// watchURL is the address of the video id that a read hands yt-dlp.
func watchURL(id string) string {
	return "https://www.youtube.com/watch?v=" + id
}

// arguments is what a read of the video id for enrichment passes yt-dlp.
func arguments(id string) []string {
	return append(paced(),
		"--skip-download",
		"--no-playlist",
		"--write-comments",
		"--extractor-args", fmt.Sprintf("youtube:max_comments=%d,%d,0,0;comment_sort=top", MaxComments, MaxComments),
		"--dump-json",
		"--",
		watchURL(id),
	)
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
// markers. Only error lines are classified, since a warning about a request
// yt-dlp retried says nothing about how the read ended. A failure yt-dlp wrote
// no error line for is named by its last line of output instead, which is where
// it writes a fault in the arguments it was passed.
func refusal(stderr string, exit error) error {
	var errorLines []string
	last := ""
	for line := range strings.Lines(stderr) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		if strings.HasPrefix(line, "ERROR:") {
			errorLines = append(errorLines, line)
		}
	}
	if len(errorLines) == 0 {
		if last == "" {
			return fmt.Errorf("yt-dlp failed and said nothing: %w", exit)
		}
		return fmt.Errorf("yt-dlp failed with no error line, last saying %q: %w", last, exit)
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
	Chapters    chapters `json:"chapters"`
	Comments    []struct {
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
	video.Chapters = read.Chapters.read()
	for _, comment := range read.Comments {
		video.Comments = append(video.Comments, comment.Text)
	}
	video.CommentsCapped = len(video.Comments) >= MaxComments
	return video, nil
}

// chapters is a video's chapters as yt-dlp's JSON writes them.
type chapters []struct {
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Title     string  `json:"title"`
}

// read is the chapters in whole seconds, and nil where there are none.
func (c chapters) read() []tracklist.Chapter {
	var read []tracklist.Chapter
	for _, chapter := range c {
		read = append(read, tracklist.Chapter{
			StartSeconds: int64(chapter.StartTime),
			EndSeconds:   int64(chapter.EndTime),
			Title:        chapter.Title,
		})
	}
	return read
}
