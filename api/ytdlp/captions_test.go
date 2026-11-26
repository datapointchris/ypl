package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/tracklist"
)

// captions is yt-dlp reading a video's caption tracks from
// testdata/captions/<id>.json, or, handed that info back, writing the track
// it names from testdata/captions/<kind>.<language>.json3.
func captions(args []string) int {
	info := argAfter(args, "--load-info-json")
	if info == "" {
		id := strings.TrimPrefix(args[len(args)-1], "https://www.youtube.com/watch?v=")
		recording, err := os.ReadFile(filepath.Join(os.Getenv(fakeTestdata), "captions", id+".json"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: [youtube] %s: no recording\n", id)
			return 1
		}
		fmt.Println(strings.ReplaceAll(string(recording), "\n", " "))
		return 0
	}
	var read struct {
		ID string `json:"id"`
	}
	body, err := os.ReadFile(info)
	if err != nil || json.Unmarshal(body, &read) != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read %s\n", info)
		return 1
	}
	kind := "manual"
	if slices.Contains(args, "--write-auto-subs") {
		kind = "automatic"
	}
	lang := argAfter(args, "--sub-langs")
	track, err := os.ReadFile(filepath.Join(os.Getenv(fakeTestdata), "captions", kind+"."+strings.TrimSuffix(lang, "-orig")+".json3"))
	if err != nil {
		return 0
	}
	out := strings.ReplaceAll(argAfter(args, "-o"), "%(id)s.%(ext)s", read.ID+"."+lang+".json3")
	if err := os.WriteFile(out, track, 0o600); err != nil {
		return 1
	}
	return 0
}

// audio is yt-dlp writing YPL_FAKE_YTDLP_AUDIO bytes of audio where -o names,
// or nothing where that is more than --max-filesize, as yt-dlp skips it.
func audio(args []string) int {
	size, _ := strconv.Atoi(os.Getenv(fakeAudio))
	if most := argAfter(args, "--max-filesize"); most != "" {
		if limit, _ := strconv.Atoi(most); size > limit {
			fmt.Printf("[download] File is larger than max-filesize (%d bytes > %d bytes). Aborting.\n", size, limit)
			return 0
		}
	}
	out := strings.ReplaceAll(argAfter(args, "-o"), "%(ext)s", "webm")
	if err := os.WriteFile(out, make([]byte, size), 0o600); err != nil {
		return 1
	}
	return 0
}

// fakeAudio is how many bytes of audio the fake writes.
const fakeAudio = "YPL_FAKE_YTDLP_AUDIO"

// argAfter is the argument after flag, and "" where flag is absent.
func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// A video with a manual English track and YouTube's automatic one answers the
// manual track's segments, with the video's chapters and details.
func TestCaptionsAnswerTheManualTrackOverTheAutomaticOne(t *testing.T) {
	got, err := reader(t, "captions").Captions(context.Background(), Service, "Wr1tt3nVid0")
	if err != nil {
		t.Fatalf("Captions: %v", err)
	}
	if got.Track == nil || *got.Track != (Track{Language: "en", Kind: Manual}) {
		t.Fatalf("track %+v, want the manual en track", got.Track)
	}
	wantSegments := []Segment{
		{StartMs: 1200, DurationMs: 2500, Text: "Welcome back to the written talk."},
		{StartMs: 3700, DurationMs: 3000, Text: "Today we pace every read."},
	}
	if !slices.Equal(got.Segments, wantSegments) {
		t.Fatalf("segments %+v, want %+v", got.Segments, wantSegments)
	}
	wantChapters := []tracklist.Chapter{{StartSeconds: 0, EndSeconds: 300, Title: "Opening"}, {StartSeconds: 300, EndSeconds: 754, Title: "The second part"}}
	if !slices.Equal(got.Chapters, wantChapters) || got.Title != "A written talk about pacing" || got.Language != "en" || got.DurationSeconds == nil || *got.DurationSeconds != 754 {
		t.Fatalf("captions %+v, want the recording's chapters, title, language and length", got)
	}
}

// The transcript is the line a shell pipeline over the same json3 track prints,
// and falls back to the automatic track where there is no manual one. A
// language the video has no track in is ErrNoCaptions.
func TestATranscriptIsTheLineTheShellPipelinePrints(t *testing.T) {
	// What `jq -r '.events[]?.segs[]?.utf8 // empty' automatic.en.json3 | tr '\n' ' ' | sed 's/  */ /g'`
	// prints.
	const printed = "so the door opens once every eight seconds [Music] "
	d := reader(t, "captions")
	got, err := d.Transcript(context.Background(), Person, "Wr1tt3nVid1", "en")
	if err != nil || got.Text != printed || got.Track != (Track{Language: "en", Kind: Automatic}) {
		t.Fatalf("Transcript = %+v, %v, want %q from the automatic en track", got, err, printed)
	}
	if _, err := d.Transcript(context.Background(), Person, "Wr1tt3nVid1", "de"); !errors.Is(err, ErrNoCaptions) {
		t.Fatalf("a transcript in a language with no track = %v, want ErrNoCaptions", err)
	}
}

func TestATrackIsPickedManualThenEnglishThenRecognizedThenTranslated(t *testing.T) {
	cases := []struct {
		name              string
		video             string
		manual, automatic []string
		want              Track
		found             bool
	}{
		{"manual in the video's language", "de", []string{"de-DE", "en"}, []string{"de-orig", "en"}, Track{"de-DE", Manual}, true},
		{"manual English", "de", []string{"en-GB"}, []string{"de-orig", "en"}, Track{"en-GB", Manual}, true},
		{"automatic in the original language", "de", nil, []string{"de", "de-orig", "en", "fr"}, Track{"de-orig", Automatic}, true},
		{"automatic English", "", nil, []string{"en", "fr"}, Track{"en", Automatic}, true},
		{"none", "de", nil, []string{"fr"}, Track{}, false},
	}
	for _, c := range cases {
		if got, found := pickTrack(c.video, c.manual, c.automatic); got != c.want || found != c.found {
			t.Errorf("%s: pickTrack = %+v, %v, want %+v, %v", c.name, got, found, c.want, c.found)
		}
	}
}

// Audio over the bound fetches nothing and is ErrTooLarge, which counts as an
// answer rather than a failure. Audio within it is a file Close removes.
func TestAudioIsAFileCloseRemovesAndAudioOverItsBoundIsTooLarge(t *testing.T) {
	t.Setenv(fakeAudio, "1000")
	d := reader(t, "audio")
	d.failures = 2
	if _, err := d.Audio(context.Background(), Service, "Wr1tt3nVid0", 999); !errors.Is(err, ErrTooLarge) || d.failures != 0 {
		t.Fatalf("audio over its bound = %v with %d failures in a row, want ErrTooLarge and none", err, d.failures)
	}
	got, err := d.Audio(context.Background(), Person, "Wr1tt3nVid0", 0)
	if err != nil || got.Size != 1000 || got.Ext != "webm" {
		t.Fatalf("Audio = %+v, %v, want 1000 bytes of webm", got, err)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the audio file after Close: %v, want it gone", err)
	}
}
