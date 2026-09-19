package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestVersionNamesTheToolAndTheBuild(t *testing.T) {
	out, err := execute(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if got, want := strings.TrimSpace(out), "ypl version dev"; got != want {
		t.Fatalf("--version = %q, want %q", got, want)
	}
}

// A bare ypl answers where things stand rather than listing what the tool can
// do: the track that is on, what the server holds, and how its last sync ended.
func TestABareYplSaysWhatIsPlayingAndWhereTheServerStands(t *testing.T) {
	f := newFixture(t, serves(map[string]string{
		"/api/v1/status": `{"library": {"playlists": 36, "videos": 1584, "unavailable_videos": 40,
			"enriched_videos": 58, "videos_with_tracklist": 51, "tracks": 465, "plays": 0}, "last_run": ` + run(2, "ok") + `, "last_ok_run": ` + run(2, "ok") + `}`,
		"/api/v1/videos/dQw4w9WgXcQ": playingVideo,
	}))
	playingMpv(t, map[string]any{
		"path":        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"time-pos":    1830.4,
		"duration":    21600.0,
		"media-title": "whatever mpv called it",
	})

	got := f.run()
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	for _, value := range []string{"Second", "Six Hours Of House", "1584", "51", "2026-09-01T00:01:00Z"} {
		if !strings.Contains(got.out, value) {
			t.Errorf("said\n%s\nwant it to carry %q", got.out, value)
		}
	}

	// A server that refuses the status leaves stdout empty, rather than
	// holding what is playing above an error.
	down := newFixture(t, refuses(http.StatusServiceUnavailable, "unavailable", "down"))
	if failed := down.run(); failed.code != 1 || failed.out != "" {
		t.Errorf("with the status refused, exited %d having written %q, want 1 and nothing", failed.code, failed.out)
	}
}
