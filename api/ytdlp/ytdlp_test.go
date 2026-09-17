package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/tracklist"
)

// fakeMode names what this test binary does when a test runs it as yt-dlp:
// answer with a recording, fail with an error text, hang, or answer as another
// video. The process a hanging fake starts sleeps. Only
// TestTheRealYtdlpTakesEveryArgumentAReadPasses runs yt-dlp itself, and it
// makes no request.
const fakeMode = "YPL_FAKE_YTDLP"

// fakeArgs is the file the fake writes its arguments to, and fakeError the
// error text it fails with.
const (
	fakeArgs  = "YPL_FAKE_YTDLP_ARGS"
	fakeError = "YPL_FAKE_YTDLP_ERROR"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeMode); mode != "" {
		os.Exit(fake(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fake is yt-dlp as a test runs it, answering a read of the video the last
// argument names from testdata/<id>.json, and its exit status.
func fake(mode string, args []string) int {
	if mode == "sleep" {
		time.Sleep(10 * time.Second)
		return 0
	}
	if path := os.Getenv(fakeArgs); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(args, "\n")), 0o600)
	}
	id := strings.TrimPrefix(args[len(args)-1], "https://www.youtube.com/watch?v=")
	switch mode {
	case "answer":
		recording, err := os.ReadFile(filepath.Join("testdata", id+".json"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: [youtube] %s: no recording\n", id)
			return 1
		}
		fmt.Println(strings.ReplaceAll(string(recording), "\n", " "))
		return 0
	case "another":
		fmt.Println(`{"id": "zzzzzzzzzzz", "title": "Another"}`)
		return 0
	case "fail":
		fmt.Fprint(os.Stderr, os.Getenv(fakeError))
		return 1
	case "hang":
		// A process yt-dlp started holds the output open after yt-dlp is killed.
		held := exec.Command(os.Args[0])
		held.Env = append(os.Environ(), fakeMode+"=sleep")
		held.Stdout = os.Stdout
		_ = held.Start()
		time.Sleep(30 * time.Second)
		return 0
	}
	return 2
}

// reader is a Reader running this test binary as yt-dlp in mode.
func reader(t *testing.T, mode string) *Reader {
	t.Helper()
	t.Setenv(fakeMode, mode)
	return &Reader{path: os.Args[0]}
}

func TestAReadReturnsTheVideoWithItsChaptersAndTopComments(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	r := reader(t, "answer")
	t.Setenv(fakeArgs, args)

	video, err := r.Video(context.Background(), "-kbYeaEP-ME")
	if err != nil {
		t.Fatalf("Video: %v", err)
	}
	if video.ID != "-kbYeaEP-ME" || video.DurationSeconds != 2483 || video.UploadDate != "2023-01-11" || video.Description == "" {
		t.Fatalf("video = %+v, want -kbYeaEP-ME of 2483 seconds uploaded 2023-01-11 with its description", video)
	}
	first := tracklist.Chapter{StartSeconds: 0, EndSeconds: 251, Title: "San Rossore"}
	if len(video.Chapters) != 9 || video.Chapters[0] != first || video.Chapters[8].EndSeconds != 2483 {
		t.Fatalf("chapters = %+v, want 9 from %+v ending at 2483", video.Chapters, first)
	}
	if !slices.Equal(video.Comments, []string{"To new beginnings.", "Such an amazing album!"}) {
		t.Fatalf("comments = %q, want the two recorded", video.Comments)
	}

	sent, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--ignore-config", "--no-plugin-dirs", "--skip-download", "--no-playlist", "--sleep-requests", "1", "--write-comments",
		"--extractor-args", "youtube:max_comments=20,20,0,0;comment_sort=top", "--dump-json", "--",
		"https://www.youtube.com/watch?v=-kbYeaEP-ME",
	}
	if got := strings.Split(string(sent), "\n"); !slices.Equal(got, want) {
		t.Fatalf("yt-dlp ran with %q, want %q", got, want)
	}
}

func TestAVideoWithNoChaptersReturnsItsCommentsInTopOrder(t *testing.T) {
	video, err := reader(t, "answer").Video(context.Background(), "Zo6Gu8RUExM")
	if err != nil {
		t.Fatalf("Video: %v", err)
	}
	if video.Chapters != nil || len(video.Comments) != 3 || !strings.HasPrefix(video.Comments[0], "Track list:") {
		t.Fatalf("chapters %+v and comments %q, want none and three opening with the pinned track list", video.Chapters, video.Comments)
	}
	if tracks := tracklist.Best(video.Chapters, video.DurationSeconds, video.Description, video.Comments); len(tracks) != 17 || tracks[16].Title != "Bad Romance (Extended Mix)" {
		t.Fatalf("tracks from the recording = %d, want the pinned comment's 17", len(tracks))
	}
}

// Each error text is how yt-dlp words YouTube's answer, and the error a read
// returns for it.
func TestAFailedReadNamesWhatYouTubeRefused(t *testing.T) {
	cases := []struct {
		name, stderr string
		want         error
	}{
		{
			name:   "the rate limit",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. This content isn't available, try again later. The current session has been rate-limited by YouTube for up to an hour.\n",
			want:   ErrRateLimited,
		},
		{
			name:   "the bot check",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication.\n",
			want:   ErrRateLimited,
		},
		{
			name:   "a private video",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Private video. Sign in if you've been granted access to this video\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a removed video",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. This video has been removed by the uploader\n",
			want:   ErrUnreadable,
		},
		{
			name:   "an age-restricted video",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Sign in to confirm your age. This video may be inappropriate for some users.\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a members-only video",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Join this channel to get access to members-only content like this video, and other exclusive perks.\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video removed for a copyright request",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. It was removed following a copyright removal request by Label\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video removed for a legal complaint",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. This video was removed due to a legal complaint\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video removed for its terms of service",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: This video has been removed for violating YouTube's Terms of Service\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video blocked by a claim",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. It was blocked due to the claimed content by Label.\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video of a terminated account",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. This video is no longer available because the YouTube account associated with this video has been terminated.\n",
			want:   ErrUnreadable,
		},
		{
			name:   "a video that is not available",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Video unavailable. This video is not available\n",
			want:   ErrUnreadable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := reader(t, "fail")
			t.Setenv(fakeError, "[youtube] Extracting URL\n"+c.stderr)
			_, err := r.Video(context.Background(), "Ljd32XdWRjY")
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), strings.TrimSpace(c.stderr)) {
				t.Fatalf("Video = %v, want %v naming yt-dlp's error line", err, c.want)
			}
		})
	}
}

// A warning about a request yt-dlp retried says nothing about how the read
// ended, so a failure after a warned 429 is neither a rate limit nor a video
// closed to reading. A bare "Video unavailable", which YouTube's rate limit
// also opens with, is neither too.
func TestAFailureOtherThanARefusalIsNeitherKind(t *testing.T) {
	cases := map[string]string{
		"a failure after a warned 429": "WARNING: [youtube] HTTP Error 429: Too Many Requests. Retrying (1/3)...\nERROR: [youtube] Ljd32XdWRjY: Unable to extract initial player response; please report this issue\n",
		"a bare Video unavailable":     "ERROR: [youtube] Ljd32XdWRjY: Video unavailable\n",
	}
	for name, stderr := range cases {
		t.Run(name, func(t *testing.T) {
			r := reader(t, "fail")
			t.Setenv(fakeError, stderr)
			_, err := r.Video(context.Background(), "Ljd32XdWRjY")
			last := strings.TrimSpace(stderr[strings.LastIndex(strings.TrimSpace(stderr), "\n")+1:])
			if err == nil || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), last) {
				t.Fatalf("Video = %v, want a failure naming %q and no refusal", err, last)
			}
		})
	}
}

func TestAReadRefusesAnIDThatIsNotAVideoAndAnAnswerForAnotherVideo(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	r := reader(t, "answer")
	t.Setenv(fakeArgs, args)
	if _, err := r.Video(context.Background(), "--exec=rm"); err == nil {
		t.Fatal("a read of --exec=rm succeeded, want it refused")
	}
	if _, err := os.Stat(args); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("yt-dlp ran for --exec=rm (%v), want it refused before yt-dlp runs", err)
	}
	if _, err := reader(t, "another").Video(context.Background(), "Ljd32XdWRjY"); err == nil {
		t.Fatal("a read answered with another video succeeded, want it refused")
	}
}

// yt-dlp is killed at the deadline while a process it started holds its output
// open for ten seconds more, and the read returns the deadline all the same.
func TestAReadEndsAtItsDeadlineWhenYtdlpsChildHoldsTheOutput(t *testing.T) {
	r := reader(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	began := time.Now()
	_, err := r.Video(ctx, "Ljd32XdWRjY")
	if took := time.Since(began); !errors.Is(err, context.DeadlineExceeded) || took > waitDelay+3*time.Second {
		t.Fatalf("Video = %v after %v, want the deadline within %v", err, took, waitDelay+3*time.Second)
	}
}

func TestNewReaderFindsTheBinaryOrSaysWhichItCouldNot(t *testing.T) {
	if _, err := NewReader(filepath.Join(t.TempDir(), "yt-dlp")); err == nil || !strings.Contains(err.Error(), "yt-dlp") {
		t.Fatalf("NewReader of a missing binary = %v, want a failure naming it", err)
	}
	if r, err := NewReader(os.Args[0]); err != nil || r.path != os.Args[0] {
		t.Fatalf("NewReader of this binary = %+v, %v", r, err)
	}
}

// Every other test runs this binary as yt-dlp, so each asserts the arguments a
// read passes against this package's own copy of them, and an option yt-dlp no
// longer has passes all of them. This hands the list to the real binary and
// requires the read to fail at its URL rather than at its options: a file URL
// is refused once every option is taken, and an option yt-dlp does not know is
// refused before that.
//
// It reaches an option renamed or removed. It does not reach a misspelled
// extractor-arg key, which yt-dlp takes in silence, leaving the extractor's own
// default in place.
func TestTheRealYtdlpTakesEveryArgumentAReadPasses(t *testing.T) {
	path, err := exec.LookPath("yt-dlp")
	if err != nil {
		t.Skipf("yt-dlp is not installed: %v", err)
	}
	args := arguments("-kbYeaEP-ME")
	args[len(args)-1] = "file:///dev/null"
	cmd := exec.CommandContext(t.Context(), path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()

	said := stderr.String()
	if strings.Contains(said, "no such option") {
		t.Fatalf("yt-dlp refused an option a read passes, running %q: %s", args, said)
	}
	if !strings.Contains(said, "file:// URLs are disabled") {
		t.Fatalf("yt-dlp ran %q and said %q (%v), want every option taken and the URL refused", args, said, err)
	}
}
