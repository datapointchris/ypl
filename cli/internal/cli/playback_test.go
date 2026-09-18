package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
)

// playingMpv listens where `ypl now` looks and answers property reads the way
// mpv does — one reply a request, matched by request_id, with an unsolicited
// event line pushed first.
//
// A real socket rather than a seam in the code, because the reply matching is
// the whole of what could be wrong here: mpv pushes events down the same
// connection, so a reader taking the first line back reads an event as the
// answer.
func playingMpv(t *testing.T, properties map[string]any) {
	t.Helper()
	socket := mpv.SocketPath()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("make the socket directory: %v", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// Accepted in a loop, as mpv does. `ypl now` is what a status bar runs on a
	// timer, so one connection a run is the normal case and a fake serving one
	// connection would leave every run after the first hanging on the dial.
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go answerAsMpv(connection, properties)
		}
	}()
}

// answerAsMpv serves one connection: an unsolicited event first, then one reply
// a request, matched by request_id.
func answerAsMpv(connection net.Conn, properties map[string]any) {
	defer func() { _ = connection.Close() }()
	// What mpv sends without being asked, which a reader matching on request_id
	// has to step over.
	_, _ = connection.Write([]byte(`{"event":"playback-restart"}` + "\n"))
	decoder := json.NewDecoder(connection)
	for {
		var asked struct {
			Command   []string `json:"command"`
			RequestID int      `json:"request_id"`
		}
		if err := decoder.Decode(&asked); err != nil {
			return
		}
		reply := map[string]any{"request_id": asked.RequestID, "error": "success"}
		if value, held := properties[asked.Command[len(asked.Command)-1]]; held {
			reply["data"] = value
		} else {
			reply["error"] = "property unavailable"
		}
		encoded, _ := json.Marshal(reply)
		_, _ = connection.Write(append(encoded, '\n'))
	}
}

const playingVideo = `{"id": "dQw4w9WgXcQ", "title": "Six Hours Of House", "channel_title": "One",
	"duration_seconds": 21600, "upload_date": null, "is_unavailable": false, "enriched_ts": "2026-01-01T00:00:00Z",
	"track_count": 3, "artists": ["Bjork"], "playlists": [], "description": null, "tracks": [
		{"position": 1, "start_seconds": 0, "end_seconds": null, "artist": "Bjork", "title": "Opener",
			"raw_text": "Bjork - Opener", "source": "chapter"},
		{"position": 2, "start_seconds": 600, "end_seconds": null, "artist": "Moby", "title": "Second",
			"raw_text": "Moby - Second", "source": "chapter"},
		{"position": 3, "start_seconds": 4000, "end_seconds": null, "artist": null, "title": "Third",
			"raw_text": "Third", "source": "comment"}]}`

// The point of the command: the server holds a tracklist with real timestamps,
// so this reports the track inside a two-hour mix rather than the name of the
// mix. The position is inside the second track and past the first.
func TestNowReportsTheTrackAtThePositionMpvIsAt(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/videos/dQw4w9WgXcQ": playingVideo}))
	playingMpv(t, map[string]any{
		"path":        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"time-pos":    1830.4,
		"duration":    21600.0,
		"media-title": "whatever mpv called it",
	})

	got := asJSON[nowPlaying](t, f.run("now", "--json"))
	switch {
	case got.VideoID != "dQw4w9WgXcQ":
		t.Fatalf("read the video as %q, want it out of mpv's path", got.VideoID)
	case got.Track == nil:
		t.Fatal("reported no track, and the position is inside the second one")
	case got.Track.Title != "Second":
		t.Errorf("reported %q, want the track the position falls in", got.Track.Title)
	case got.PositionSeconds == nil || *got.PositionSeconds != 1830:
		t.Errorf("position = %v, want mpv's own, in whole seconds", got.PositionSeconds)
	case got.Title != "Six Hours Of House":
		t.Errorf("title = %q, want the server's rather than mpv's", got.Title)
	}

	// The plain rendering names the track and the video, which is what a status
	// bar puts on screen.
	plain := f.run("now")
	if !strings.Contains(plain.out, "Moby - Second") || !strings.Contains(plain.out, "Six Hours Of House") {
		t.Errorf("printed %q, want the track and the video", plain.out)
	}

	// The edges of the same question, which one run of the command cannot
	// reach. start_seconds is nullable in the store, so a track whose start the
	// tracklist does not carry arrives here and cannot hold a position. mpv
	// answers no time-pos until playback has started, so a nil position is the
	// ordinary first read rather than a broken one.
	at := func(seconds int64) *int64 { return &seconds }
	edges := []api.Track{
		{Position: 1, Title: "No start"},
		{Position: 2, StartSeconds: at(100), EndSeconds: at(200), Title: "Bounded"},
		{Position: 3, StartSeconds: at(200), Title: "Last"},
	}
	for _, edge := range []struct {
		why      string
		position *int64
		want     string
	}{
		{"a track with no start cannot be known to hold a position", at(50), ""},
		{"a position inside a bounded track is that track", at(150), "Bounded"},
		{"an end is where the track stops, so the position on it is the next one", at(200), "Last"},
		{"the last track runs to the end of the video", at(9000), "Last"},
		{"nothing has a position before playback starts", nil, ""},
	} {
		name := ""
		if got := trackAt(edges, edge.position); got != nil {
			name = got.Title
		}
		if name != edge.want {
			t.Errorf("%s: reported %q, want %q", edge.why, name, edge.want)
		}
	}
}

// Exits 1 rather than failing, so a status bar can run it unguarded.
func TestNowExitsOneWithNothingPlaying(t *testing.T) {
	f := newFixture(t, serves(nil))

	got := f.run("now")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if got.out != "" {
		t.Errorf("stdout = %q, want nothing on the data stream", got.out)
	}
	if !hinted.MatchString(got.err) {
		t.Errorf("stderr = %q, want it to name what would put something on", got.err)
	}
	if len(f.sent) != 0 {
		t.Errorf("asked the server %d times with nothing playing", len(f.sent))
	}
}

// A play is keyed by the client so a retry stores one row, and the video is
// named by an id or by a link somebody copied.
func TestPlaysAddSendsAVersion7IdAndTheVideoFromAURL(t *testing.T) {
	stored := `{"id": "01920000-0000-7000-8000-000000000000", "handle": 41, "played_ts": "2026-09-17T12:00:00Z",
		"video": {"id": "dQw4w9WgXcQ", "title": "Six Hours Of House", "channel_title": "One"}}`
	f := newFixture(t, answers(map[string]answer{"POST /api/v1/plays": {body: stored}}))

	got := asJSON[api.Play](t, f.run("plays", "add", "https://youtu.be/dQw4w9WgXcQ?t=42", "--json"))
	if got.Handle != 41 {
		t.Fatalf("answered %+v, want the play the server stored", got)
	}
	var body struct {
		ID      string `json:"id"`
		VideoID string `json:"video_id"`
	}
	sent := f.onlyWrite()
	if err := json.Unmarshal([]byte(sent.Body), &body); err != nil {
		t.Fatalf("the body %q does not decode: %v", sent.Body, err)
	}
	if body.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("sent %q, want the id out of the address", body.VideoID)
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id.Version() != 7 {
		t.Errorf("sent id %q, which the server refuses unless it is a version 7 UUID: %v", body.ID, err)
	}

	// A token that names no video never reaches the server.
	f = newFixture(t, answers(nil))
	if refused := f.run("plays", "add", "not a video"); refused.code != 2 || len(f.sent) != 0 {
		t.Errorf("exited %d after %d requests, want 2 and none", refused.code, len(f.sent))
	}
}

// The limit is a ceiling on what plays, so it is applied before the unavailable
// ones are counted — otherwise the count reports videos dropped from a part of
// the playlist that was never going to play.
func TestPlayableCountsWhatItDroppedFromThePartThatWouldPlay(t *testing.T) {
	playlist := api.Playlist{Items: []api.PlaylistItem{
		{Video: api.VideoSummary{ID: "a"}},
		{Video: api.VideoSummary{ID: "gone", IsUnavailable: true}},
		{Video: api.VideoSummary{ID: "b"}},
		{Video: api.VideoSummary{ID: "far", IsUnavailable: true}},
		{Video: api.VideoSummary{ID: "c"}},
	}}

	urls, left := playable(playlist, 0)
	if want := []string{watchURL("a"), watchURL("b"), watchURL("c")}; !slices.Equal(urls, want) {
		t.Errorf("played %v, want the three YouTube serves", urls)
	}
	if left != 2 {
		t.Errorf("left out %d, want both unavailable", left)
	}

	urls, left = playable(playlist, 2)
	if want := []string{watchURL("a"), watchURL("b")}; !slices.Equal(urls, want) {
		t.Errorf("played %v under a limit of 2, want %v", urls, want)
	}
	if left != 1 {
		t.Errorf("left out %d, want only the one inside the limit", left)
	}
}
