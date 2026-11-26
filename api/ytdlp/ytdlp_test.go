package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// fakeTestdata is this package's testdata directory, which the fake reads
// from wherever a read runs it.
const fakeTestdata = "YPL_FAKE_YTDLP_TESTDATA"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeMode); mode != "" {
		os.Exit(fake(mode, os.Args[1:]))
	}
	testdata, err := filepath.Abs("testdata")
	if err == nil {
		err = os.Setenv(fakeTestdata, testdata)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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
	case "stamp":
		return stamp(id)
	case "captions":
		return captions(args)
	case "audio":
		return audio(args)
	case "answer":
		recording, err := os.ReadFile(filepath.Join(os.Getenv(fakeTestdata), id+".json"))
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
		held := exec.CommandContext(context.Background(), os.Args[0])
		held.Env = append(os.Environ(), fakeMode+"=sleep")
		held.Stdout = os.Stdout
		_ = held.Start()
		time.Sleep(30 * time.Second)
		return 0
	}
	return 2
}

// reader is a door running this test binary as yt-dlp in mode, with no gap
// between reads.
func reader(t *testing.T, mode string) *Door {
	t.Helper()
	t.Setenv(fakeMode, mode)
	return openDoor(t, &memoryPauses{})
}

// openDoor is a door running this test binary as yt-dlp, keeping its pauses in
// pauses, with no gap between reads.
func openDoor(t *testing.T, pauses Pauses) *Door {
	t.Helper()
	d, err := NewDoor(context.Background(), Binary{path: os.Args[0]}, pauses, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open the door: %v", err)
	}
	d.gap = func() time.Duration { return 0 }
	d.nextStart = time.Time{}
	return d
}

// memoryPauses keeps a door's pauses in memory.
type memoryPauses struct {
	mu       sync.Mutex
	recorded []Pause
}

func (m *memoryPauses) LatestPause(context.Context) (Pause, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recorded) == 0 {
		return Pause{}, false, nil
	}
	return m.recorded[len(m.recorded)-1], true, nil
}

func (m *memoryPauses) RecordPause(_ context.Context, pause Pause) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recorded = append(m.recorded, pause)
	return nil
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
	want := []string{"--", "https://www.youtube.com/watch?v=-kbYeaEP-ME"}
	if got := strings.Split(string(sent), "\n"); !slices.Equal(got[len(got)-2:], want) {
		t.Fatalf("yt-dlp ran with %q, want it to end on %q, so an id opening with a hyphen is no option", got, want)
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
		{
			name:   "a premiere still to come",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: Premieres in 3 hours\n",
			want:   ErrNotStarted,
		},
		{
			name:   "a scheduled live stream",
			stderr: "ERROR: [youtube] Ljd32XdWRjY: This live event will begin in 2 days.\n",
			want:   ErrNotStarted,
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

// yt-dlp writes a fault in the arguments it was given without an ERROR: line,
// and that sentence is the whole diagnosis. An operator whose yt-dlp no longer
// takes an option a read passes sees it on every read of the run.
func TestAFailureWithNoErrorLineIsNamedByYtdlpsLastWords(t *testing.T) {
	r := reader(t, "fail")
	t.Setenv(fakeError, "Usage: yt-dlp [OPTIONS] URL [URL...]\n\nyt-dlp: error: no such option: --write-comments\n")

	_, err := r.Video(context.Background(), "Ljd32XdWRjY")
	if err == nil || !strings.Contains(err.Error(), "no such option: --write-comments") {
		t.Fatalf("Video = %v, want it naming what yt-dlp said last", err)
	}
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("Video = %v, want a fault in the arguments read as neither refusal", err)
	}
}

// A read caps the comments it asks for, so a video with more says so: what the
// read did not return is the rest of them rather than nothing.
func TestAReadSaysWhenItCappedTheCommentsItReturned(t *testing.T) {
	comments := make([]string, MaxComments)
	for i := range comments {
		comments[i] = fmt.Sprintf(`{"text": "comment %d"}`, i)
	}
	capped, err := decode(fmt.Appendf(nil, `{"id": "Ljd32XdWRjY", "comments": [%s]}`, strings.Join(comments, ",")))
	if err != nil || !capped.CommentsCapped || len(capped.Comments) != MaxComments {
		t.Fatalf("a read of %d comments = %+v, %v, want it capped", MaxComments, capped, err)
	}
	under, err := decode(fmt.Appendf(nil, `{"id": "Ljd32XdWRjY", "comments": [%s]}`, strings.Join(comments[:MaxComments-1], ",")))
	if err != nil || under.CommentsCapped {
		t.Fatalf("a read of %d comments = %+v, %v, want it uncapped", MaxComments-1, under, err)
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

func TestFindFindsTheBinaryOrSaysWhichItCouldNot(t *testing.T) {
	if _, err := Find(filepath.Join(t.TempDir(), "yt-dlp")); err == nil || !strings.Contains(err.Error(), "yt-dlp") {
		t.Fatalf("Find of a missing binary = %v, want a failure naming it", err)
	}
	if b, err := Find(os.Args[0]); err != nil || b.path != os.Args[0] {
		t.Fatalf("Find of this binary = %+v, %v", b, err)
	}
}

// Every other test runs this binary as yt-dlp, so each asserts the arguments a
// read passes against this package's own copy of them, and an option yt-dlp no
// longer has passes all of them. This hands each read's list to the real binary
// and requires every option taken: a read of a URL fails at the URL, since a
// file URL is refused once every option is taken and an option yt-dlp does not
// know is refused before that, and the read of a caption track fails at the
// info file it is pointed at.
//
// It reaches an option renamed or removed. It does not reach a misspelled
// extractor-arg key, which yt-dlp takes in silence, leaving the extractor's own
// default in place.
func TestTheRealYtdlpTakesEveryArgumentAReadPasses(t *testing.T) {
	path, err := exec.LookPath("yt-dlp")
	if err != nil {
		t.Skipf("yt-dlp is not installed: %v", err)
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	reads := map[string][]string{
		"enrichment": arguments("-kbYeaEP-ME"),
		"captions":   infoArguments("-kbYeaEP-ME"),
		"audio":      audioArguments(dir, "-kbYeaEP-ME", 1000),
		"history":    historyArguments(filepath.Join(dir, "cookies.txt"), 5),
		"track":      trackArguments(missing, dir, Track{Language: "en", Kind: Automatic}),
	}
	for name, args := range reads {
		t.Run(name, func(t *testing.T) {
			refusal := "file:// URLs are disabled"
			if name == "track" {
				refusal = "missing.json"
			} else {
				args[len(args)-1] = "file:///dev/null"
			}
			cmd := exec.CommandContext(t.Context(), path, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			said := stderr.String()
			if strings.Contains(said, "no such option") {
				t.Fatalf("yt-dlp refused an option a read passes, running %q: %s", args, said)
			}
			if !strings.Contains(said, refusal) {
				t.Fatalf("yt-dlp ran %q and said %q (%v), want every option taken and %q", args, said, err, refusal)
			}
		})
	}
}
