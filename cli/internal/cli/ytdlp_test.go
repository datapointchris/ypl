package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// written is when the files in a run's answer were written.
var written = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// runAnswer is a run's answer as the server writes it: what yt-dlp printed,
// the code it exited with, and each file it wrote, as one tar.
func runAnswer(t *testing.T, code int, files map[string]string) http.HandlerFunc {
	t.Helper()
	entries := [][2]string{
		{"stdout", "[info] Writing video subtitles to: x.en.vtt\n"},
		{"stderr", "WARNING: a warning yt-dlp gave\n"},
		{"exit-code", fmt.Sprintf("%d\n", code)},
	}
	for name, content := range files {
		entries = append(entries, [2]string{"files/" + name, content})
	}
	var answer bytes.Buffer
	archive := tar.NewWriter(&answer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry[0], Mode: 0o644, Size: int64(len(entry[1])), ModTime: written, Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(answer.Bytes())
	}
}

// The command is yt-dlp run elsewhere: what it typed is sent as typed, what
// yt-dlp printed is printed on the stream it printed on, the caption file
// lands in this directory under the name -o gave it, and the exit code is
// yt-dlp's.
func TestYtdlpWritesWhatTheRunWroteHereAndExitsWithItsCode(t *testing.T) {
	f := newFixture(t, runAnswer(t, 1, map[string]string{"x.en.vtt": "WEBVTT\n"}))
	here := t.TempDir()
	t.Chdir(here)
	typed := []string{"--skip-download", "--write-auto-subs", "-o", "x.%(ext)s", "https://youtu.be/Wr1tt3nVid0"}
	got := f.run(append([]string{"ytdlp", "--"}, typed...)...)
	if got.code != 1 || !strings.Contains(got.out, "Writing video subtitles") || !strings.Contains(got.err, "WARNING: a warning") {
		t.Fatalf("exited %d, printing %q and %q; want yt-dlp's 1 and each stream on its own", got.code, got.out, got.err)
	}
	caption, err := os.ReadFile(filepath.Join(here, "x.en.vtt"))
	if err != nil || string(caption) != "WEBVTT\n" {
		t.Fatalf("x.en.vtt holds %q (%v), want the run's caption file", caption, err)
	}
	if info, err := os.Stat(filepath.Join(here, "x.en.vtt")); err != nil || !info.ModTime().Equal(written) {
		t.Errorf("x.en.vtt was written at %v (%v), want yt-dlp's %v", info.ModTime(), err, written)
	}
	sent := f.onlyWrite()
	var body struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(sent.Body), &body); err != nil || sent.URL.Path != "/api/v1/ytdlp/runs" || !slices.Equal(body.Args, typed) {
		t.Errorf("sent %s %s, want %q to /api/v1/ytdlp/runs", sent.URL.Path, sent.Body, typed)
	}

	// A refused argument is the caller's to change, so it is a usage error, and
	// the server's sentence naming it reaches the terminal.
	f = newFixture(t, refuses(http.StatusUnprocessableEntity, "ytdlp_argument_refused",
		"ypl does not pass --cookies to yt-dlp: a signed-in read risks the account"))
	refused := f.run("ytdlp", "--", "--cookies", "c.txt", "https://youtu.be/Wr1tt3nVid0")
	if refused.code != 2 || !strings.Contains(refused.err, "--cookies") || !strings.Contains(refused.err, "risks the account") {
		t.Errorf("a refused cookie jar exited %d saying %q, want 2 naming --cookies and why", refused.code, refused.err)
	}

	// An answer naming a file outside this directory writes nothing there.
	f = newFixture(t, runAnswer(t, 0, map[string]string{"../escaped": "x"}))
	if escaped := f.run("ytdlp", "--", "https://youtu.be/Wr1tt3nVid0"); escaped.code != 1 || !strings.Contains(escaped.err, "outside this directory") {
		t.Errorf("an answer climbing out exited %d saying %q, want it refused", escaped.code, escaped.err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(here), "escaped")); err == nil {
		t.Error("the answer wrote outside the directory the command ran in")
	}
}
