package handlers

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/ytdlp"
)

// writesCaptions is yt-dlp writing a video's automatic English captions where
// its output template names, saying so on stdout and warning on stderr, then
// exiting with FAKE_EXIT. It writes its arguments to YPL_TEST_ARGV.
const writesCaptions = `for a in "$@"; do case "$a" in --output=*) out="${a#--output=}";; esac; done
file=$(printf '%s' "$out" | sed 's/%(ext)s/en.vtt/')
printf 'WEBVTT\n\n00:00.000 --> 00:02.000\na line someone wrote\n' > "$file"
printf '%s\n' "$@" > "$YPL_TEST_ARGV"
echo "[info] Writing video subtitles to: $file"
echo "WARNING: a warning yt-dlp gave" >&2
exit "${FAKE_EXIT:-0}"
`

// scriptDoor is a real door running script as yt-dlp, a millisecond between
// reads, and the file the script writes its arguments to.
func scriptDoor(t *testing.T, script string) (*ytdlp.Door, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(dir, "argv")
	t.Setenv("YPL_TEST_ARGV", argv)
	binary, err := ytdlp.Find(path)
	if err != nil {
		t.Fatal(err)
	}
	pace := ytdlp.Pace{Gap: time.Millisecond, Sleeps: ytdlp.DefaultPace.Sleeps}
	door, err := ytdlp.NewDoor(context.Background(), binary, pace, &heldPauses{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(door.Close)
	return door, argv
}

// heldPauses keeps a door's pauses in memory.
type heldPauses struct {
	mu     sync.Mutex
	latest *ytdlp.Pause
}

func (p *heldPauses) LatestPause(context.Context) (ytdlp.Pause, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest == nil {
		return ytdlp.Pause{}, false, nil
	}
	return *p.latest, true, nil
}

func (p *heldPauses) RecordPause(_ context.Context, pause ytdlp.Pause) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latest = &pause
	return nil
}

// tarEntries is each entry of a tar answer, by name, in order.
func tarEntries(t *testing.T, body io.Reader) ([]string, map[string]string) {
	t.Helper()
	var names []string
	held := map[string]string{}
	archive := tar.NewReader(body)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return names, held
		}
		if err != nil {
			t.Fatalf("read the tar answer: %v", err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		names = append(names, header.Name)
		held[header.Name] = string(content)
	}
}

// postAt sends body to target as a person, naming host.
func (f *fixture) postAt(host, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+host+target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

const captionRun = `{"args": ["--skip-download", "--write-auto-subs", "-o", "x.%(ext)s", "https://www.youtube.com/watch?v=Wr1tt3nVid0"]}`

// A person's run answers what yt-dlp printed, the code it exited with, and
// the caption file it wrote, as one tar. yt-dlp is handed the options in their
// one long spelling, the door's sleeps, YouTube's extractors alone, and the
// link after --.
func TestARunAnswersItsStreamsExitCodeAndFilesAsOneTar(t *testing.T) {
	f := newFixture(t)
	door, argv := scriptDoor(t, writesCaptions)
	f.h.ReadVideosWith(door, "ypl.example.com")
	t.Setenv("FAKE_EXIT", "1")

	rec := f.postAt("ypl", "/api/v1/ytdlp/runs", captionRun)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("the run answered %d as %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	names, held := tarEntries(t, rec.Body)
	if want := []string{"stdout", "stderr", "exit-code", "files/x.en.vtt", "end"}; !slices.Equal(names, want) {
		t.Fatalf("the answer holds %v, want %v", names, want)
	}
	if !strings.Contains(held["stdout"], "Writing video subtitles to: x.en.vtt") || !strings.Contains(held["stderr"], "WARNING") ||
		held["exit-code"] != "1\n" || !strings.Contains(held["files/x.en.vtt"], "a line someone wrote") {
		t.Fatalf("the answer holds %q", held)
	}
	handed, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	want := "--skip-download\n--write-auto-subs\n--output=x.%(ext)s\n--ignore-config\n--no-plugin-dirs\n" +
		"--sleep-requests\n2\n--sleep-subtitles\n5\n--sleep-interval\n8\n--max-sleep-interval\n16\n" +
		"--use-extractors=youtube.*\n--color=never\n--\nhttps://www.youtube.com/watch?v=Wr1tt3nVid0\n"
	if string(handed) != want {
		t.Fatalf("yt-dlp was handed\n%s\nwant\n%s", handed, want)
	}

	body, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	pinned(t, "ytdlp-run", "a run's tar entries", body)
}

// A cookie jar is refused before the door, naming the option, and yt-dlp never
// runs.
func TestARunWithACookieJarIsRefusedNamingIt(t *testing.T) {
	f := newFixture(t)
	door, argv := scriptDoor(t, writesCaptions)
	f.h.ReadVideosWith(door, "ypl.example.com")

	rec := f.postAt("ypl", "/api/v1/ytdlp/runs", `{"args": ["--cookies", "c.txt", "https://youtu.be/Wr1tt3nVid0"]}`)
	body := decode[wireRefusal](t, rec, http.StatusUnprocessableEntity)
	if body.Code != "ytdlp_argument_refused" || body.Field != "--cookies" {
		t.Fatalf("refused with %+v, want --cookies named", body)
	}
	if _, err := os.Stat(argv); err == nil {
		t.Fatal("yt-dlp ran, and the run was refused")
	}
}

// Through the public address, no download is larger than the cap, and an
// answer larger than it is refused rather than sent.
func TestARunThroughThePublicAddressIsCapped(t *testing.T) {
	f := newFixture(t)
	door, argv := scriptDoor(t, writesCaptions)
	f.h.ReadVideosWith(door, "ypl.example.com")
	f.h.videos.maxPublic = 40

	refused(t, f.postAt("ypl.example.com", "/api/v1/ytdlp/runs", captionRun), http.StatusUnprocessableEntity, "answer_too_large")
	handed, err := os.ReadFile(argv)
	if err != nil || !strings.Contains(string(handed), "--max-filesize=40\n") {
		t.Fatalf("yt-dlp was handed %q (%v), want --max-filesize=40", handed, err)
	}
}

// Through the public address, a run that outlasts the budget is refused with
// a code the CLI does not send again. A second run would read YouTube from the
// start and be cut at the same point.
func TestARunPastThePublicBudgetIsRefusedAsTooLongRatherThanBusy(t *testing.T) {
	f := newFixture(t)
	door, _ := scriptDoor(t, "sleep 5\n")
	f.h.ReadVideosWith(door, "ypl.example.com")
	f.h.videos.budget = 300 * time.Millisecond

	rec := f.postAt("ypl.example.com", "/api/v1/ytdlp/runs", captionRun)
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusUnprocessableEntity || body.Code != "run_past_public_budget" {
		t.Fatalf("a run past the budget answered %d: %s", rec.Code, rec.Body)
	}
}
