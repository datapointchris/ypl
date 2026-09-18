package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// shortStateDir points XDG_STATE_HOME somewhere a unix socket address fits.
//
// The fixture's own state directory is a t.TempDir, which is short enough under
// a Linux TMPDIR and is not under the one macOS hands a test —
// /var/folders/<2>/<32>/T is 48 bytes before the test's own name is added, and
// the socket path then passes the 104-byte address limit and every bind fails
// with `invalid argument`. Building it under /tmp keeps the suite running on
// both.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ypl")
	if err != nil {
		t.Fatalf("make a short state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_STATE_HOME", dir)
	if socket := mpv.SocketPath(); !mpv.Addressable(socket) {
		t.Fatalf("%s is %d bytes, which is past what a unix socket address holds", socket, len(socket))
	}
	return dir
}

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
	shortStateDir(t)
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

// stubMpv puts a fake mpv first on PATH and returns the file it writes its
// argument list to. exit is the status it ends with.
//
// A stub rather than the real player, because what `ypl play` is
// responsible for is the command line it builds — the socket flag, --no-video,
// every pass-through argument and the URL list in order. Nothing else captures
// that, and running real mpv would reach YouTube.
func stubMpv(t *testing.T, exit int) string {
	t.Helper()
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argv + "\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mpv"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv
}

// playingVideo carries the matrix the tracklist can arrive in: two tracks the
// store placed with an end, one it placed without, and one it could not place
// at all. start_seconds is nullable in the store, so the unplaceable track is a
// real shape rather than a hypothetical.
const playingVideo = `{"id": "dQw4w9WgXcQ", "title": "Six Hours Of House", "channel_title": "One",
	"duration_seconds": 21600, "upload_date": null, "is_unavailable": false, "enriched_ts": "2026-01-01T00:00:00Z",
	"track_count": 4, "artists": ["Bjork"], "playlists": [], "description": null, "tracks": [
		{"position": 1, "start_seconds": 0, "end_seconds": 600, "artist": "Bjork", "title": "Opener",
			"raw_text": "Bjork - Opener", "source": "chapter"},
		{"position": 2, "start_seconds": 600, "end_seconds": 3600, "artist": "Moby", "title": "Second",
			"raw_text": "Moby - Second", "source": "chapter"},
		{"position": 3, "start_seconds": null, "end_seconds": null, "artist": null, "title": "Unplaceable",
			"raw_text": "Unplaceable", "source": "comment"},
		{"position": 4, "start_seconds": 4000, "end_seconds": null, "artist": null, "title": "Third",
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
		"speed":       1.5,
		"pid":         4242.0,
	})

	// The recorder tells its own player from another by the pid and bounds a
	// move by the speed, so both have to arrive decoded, or nothing is counted.
	if state, err := mpv.Read(mpv.SocketPath()); err != nil || state.PID == nil || *state.PID != 4242 ||
		state.Speed == nil || *state.Speed != 1.5 {
		t.Errorf("read the player as %+v (%v), want its pid and its speed", state, err)
	}

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

	// The whole line, not a substring of it. A position half an hour in and a
	// mix six hours long put a duration either side of an hour in one sentence,
	// which is where a clock that stopped carrying the hour is caught.
	plain := f.run("now")
	if !strings.Contains(plain.out, "Moby - Second") {
		t.Errorf("printed %q, want the track the position falls in", plain.out)
	}
	if !strings.Contains(plain.out, "Six Hours Of House  30:30 / 6:00:00") {
		t.Errorf("printed %q, want the video and how far into it", plain.out)
	}

	// The edges of the same question, which one run of the command cannot
	// reach. A track the store could not place sits between two it did, and
	// taking the first match rather than the latest would leave the track above
	// it bounded by nothing and reported for every later position.
	at := func(seconds int64) *int64 { return &seconds }
	edges := []api.Track{
		{Position: 1, StartSeconds: at(0), Title: "Opener"},
		{Position: 2, Title: "Unplaceable"},
		{Position: 3, StartSeconds: at(3600), Title: "Third"},
		{Position: 4, StartSeconds: at(7200), EndSeconds: at(7800), Title: "Bounded"},
	}
	for _, edge := range []struct {
		why      string
		position *int64
		want     string
	}{
		{"an unplaceable track never bounds the one above it", at(5400), "Third"},
		{"the latest-starting track that holds the position wins", at(1800), "Opener"},
		{"a track with no start cannot be known to hold a position", at(-1), ""},
		{"a position inside a bounded track is that track", at(7500), "Bounded"},
		{"an end is where a track stops, so the one still running takes the position on it", at(7800), "Third"},
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

// Exits 1 rather than failing, so a status bar can run it unguarded — and
// writes a document first in both modes, because the caller that reads the
// empty answer off the exit code is the one parsing the JSON.
func TestNowExitsOneWithNothingPlaying(t *testing.T) {
	f := newFixture(t, serves(nil))
	shortStateDir(t)

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

	// `ypl next --json` and `ypl auth status --json` both answer a document at
	// exit 1. Writing nothing hands a status bar a parse error where the other
	// two hand it an answer.
	empty := f.run("now", "--json")
	if empty.code != 1 {
		t.Fatalf("--json exited %d, want 1", empty.code)
	}
	var document nowPlaying
	decodeInto(t, empty.out, &document)
	if document.VideoID != "" {
		t.Error("--json named a video with nothing playing")
	}

	// An mpv that is running and holds no file answers every property and has
	// no path. It is reachable without asking: `ypl play` runs mpv
	// with the person's own config, and `idle=yes` in it leaves the player up
	// after the last video.
	playingMpv(t, map[string]any{"duration": 600.0})
	idle := f.run("now")
	if idle.code != 1 {
		t.Errorf("a running mpv with nothing loaded exited %d, want 1", idle.code)
	}
	if idle.out != "" {
		t.Errorf("stdout = %q, want nothing rather than a line of punctuation", idle.out)
	}

	// A socket that answers and then goes away is not nothing playing. Saying
	// so would make the command wrong about the one thing it is asked, and a
	// status bar would show an empty slot for a mix that is still on.
	deaf := newFixture(t, serves(nil))
	deafSocket(t)
	broken := deaf.run("now")
	if strings.Contains(broken.err, "Nothing is playing") {
		t.Error("a socket that would not answer was reported as nothing playing")
	}
	if !strings.Contains(broken.err, "could not be read") {
		t.Errorf("said %q, want it to say the socket would not answer", broken.err)
	}
}

// deafSocket listens where `ypl now` looks and closes every connection without
// answering, which is what a socket held by something that is not mpv does.
func deafSocket(t *testing.T) {
	t.Helper()
	shortStateDir(t)
	socket := mpv.SocketPath()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("make the socket directory: %v", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = connection.Write([]byte(`{"event":"playback-restart"}` + "\n"))
			_ = connection.Close()
		}
	}()
}

// A play is keyed by the client so a retry stores one row, whether it came from
// `plays add` naming a video by a link or from playback counting one as heard,
// and it is taken back by deleting it.
func TestAPlayIsRecordedOnceUnderAnIdTheClientMadeAndTakenBack(t *testing.T) {
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

	// A play is taken back by deleting it. Where nobody can answer the
	// question, nothing is asked of the server. Somebody at a terminal is
	// asked, and only a yes deletes. The delete names the play by the id the
	// read answered with, so the handle typed cannot reach another play
	// between the two.
	f = newFixture(t, answers(map[string]answer{
		"GET /api/v1/plays/41": {body: stored},
		"DELETE /api/v1/plays/01920000-0000-7000-8000-000000000000": {status: http.StatusNoContent},
	}))
	if refused := f.run("plays", "delete", "41"); refused.code != 2 || len(f.sent) != 0 {
		t.Errorf("with nobody to ask, exited %d after %d requests, want 2 and none", refused.code, len(f.sent))
	}
	f.atTerminal("n\n")
	if declined := f.run("plays", "delete", "41"); declined.code != 1 || len(f.writes()) != 0 {
		t.Errorf("answered no, exited %d after %d writes, want 1 and none", declined.code, len(f.writes()))
	}
	f.atTerminal("y\n")
	if deleted := f.run("plays", "delete", "41"); deleted.code != 0 {
		t.Fatalf("answered yes, exited %d: %s", deleted.code, deleted.err)
	}
	if sent := f.onlyWrite(); sent.Method != http.MethodDelete || sent.URL.Path != "/api/v1/plays/01920000-0000-7000-8000-000000000000" {
		t.Errorf("sent %s %s, want the delete by the play's id", sent.Method, sent.URL.Path)
	}
	f.pipe("")
	if deleted := f.run("plays", "delete", "41", "--yes"); deleted.code != 0 || len(f.writes()) != 2 {
		t.Errorf("delete --yes exited %d with %d writes in all, want 0 and a second delete: %s", deleted.code, len(f.writes()), deleted.err)
	}
	// A play already deleted is gone as asked, so deleting it succeeds and
	// sends nothing.
	f = newFixture(t, answers(map[string]answer{
		"GET /api/v1/plays/42": {status: http.StatusGone, body: `{"error": "play 42 was deleted", "code": "play_deleted"}`},
	}))
	if again := f.run("plays", "delete", "42", "--yes"); again.code != 0 || len(f.writes()) != 0 || !strings.Contains(again.err, "already deleted") {
		t.Errorf("deleting a deleted play exited %d after %d writes saying %q, want 0, none, and that it was deleted",
			again.code, len(f.writes()), again.err)
	}

	// Playback counts a mix as heard once it has played long enough, counting
	// only ground covered at the speed of playing, and only on the player this
	// playback started. The server refuses some plays: one once, one until
	// playback ends, and one always. It answers one as deleted, as it does a
	// play whose first send landed unanswered and was then taken back.
	var ending atomic.Bool
	refusedOnce := false
	f = newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		var asked struct {
			VideoID string `json:"video_id"`
		}
		_ = json.Unmarshal([]byte(f.sent[len(f.sent)-1].Body), &asked)
		refuse := false
		switch asked.VideoID {
		case "aaaaaaaaaaa":
			refuse, refusedOnce = !refusedOnce, true
		case "xxxxxxxxxxx":
			refuse = !ending.Load()
		case "yyyyyyyyyyy":
			refuse = true
		case "zzzzzzzzzzz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error": "play was deleted", "code": "play_deleted"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "down", "code": "unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": "x", "handle": 1, "played_ts": "2026-09-18T00:00:00Z",
			"video": {"id": "` + asked.VideoID + `", "title": "` + asked.VideoID + `", "channel_title": "One"}}`))
	})
	client, err := f.app.client(context.Background())
	if err != nil {
		t.Fatalf("the fixture's client: %v", err)
	}

	mine, theirs := int64(7), int64(99)
	clock := time.Unix(0, 0)
	var script []mpv.State
	var answeredAt []time.Time
	read := func(videoID string, pid int64, speed float64, length, position int64, after time.Duration) {
		clock = clock.Add(after)
		state := mpv.State{Path: youtube.WatchURL(videoID), Position: &position, Duration: &length, Speed: &speed, PID: &pid}
		script, answeredAt = append(script, state), append(answeredAt, clock)
	}
	played := func(videoID string, pid int64, speed float64, length, from, to int64) {
		for position := from; position <= to; position += int64(float64(readEvery/time.Second) * speed) {
			read(videoID, pid, speed, length, position, readEvery)
		}
	}
	played("aaaaaaaaaaa", mine, 1, 7200, 0, 600) // a two-hour mix, past twenty minutes,
	read("aaaaaaaaaaa", mine, 1, 7200, 630, 30*time.Second)
	played("aaaaaaaaaaa", mine, 1, 7200, 640, 1200)  // reaching the bar only with the late read
	played("bbbbbbbbbbb", mine, 1, 7200, 0, 20)      // skimmed: a moment, a seek,
	played("bbbbbbbbbbb", mine, 1, 7200, 6000, 6020) // and a moment
	played("ccccccccccc", mine, 1, 600, 0, 310)      // ten minutes, played past half
	played("rrrrrrrrrrr", mine, 1, 7200, 0, 1190)    // rewound just short of the bar,
	played("rrrrrrrrrrr", mine, 1, 7200, 0, 10)      // then played on past it
	played("ddddddddddd", mine, 2, 7200, 0, 1240)    // at double speed
	played("ooooooooooo", theirs, 1, 7200, 0, 1300)  // another player's, on the same socket
	played("zzzzzzzzzzz", mine, 1, 7200, 0, 1210)    // deleted as it was sent
	played("xxxxxxxxxxx", mine, 1, 7200, 0, 1210)    // refused until playback ends
	played("yyyyyyyyyyy", mine, 1, 7200, 0, 1210)    // refused always

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	next := 0
	answer := func() (mpv.State, error) {
		if next == len(script) {
			return mpv.State{}, mpv.ErrNotPlaying
		}
		next++
		return script[next-1], nil
	}
	recording := make(chan delivered, 1)
	go func() {
		recording <- recordListens(ctx, client, fromPlayer(answer, func() int64 { return mine }), ticks,
			func() time.Time { return answeredAt[next-1] })
	}()
	// One tick past the script reads nothing playing and sends nothing, and
	// its arrival means the last scripted read is fully handled. So the play
	// still refused at that point is the one the last attempt has to send.
	for range len(script) + 1 {
		ticks <- time.Now()
	}
	ending.Store(true)
	cancel()
	result := <-recording

	storedVideos := make([]string, len(result.stored))
	for i, play := range result.stored {
		storedVideos[i] = play.Video.ID
	}
	slices.Sort(storedVideos)
	if want := []string{"aaaaaaaaaaa", "ccccccccccc", "ddddddddddd", "rrrrrrrrrrr", "xxxxxxxxxxx"}; !slices.Equal(storedVideos, want) {
		t.Errorf("stored plays of %v, want %v", storedVideos, want)
	}
	if len(result.unsent) != 1 || result.unsent[0].videoID != "yyyyyyyyyyy" {
		t.Errorf("left %+v unsent, want the one the server always refused", result.unsent)
	}
	sentFor := map[string][]string{}
	for _, one := range f.writes() {
		var body struct {
			ID      string `json:"id"`
			VideoID string `json:"video_id"`
		}
		decodeInto(t, one.Body, &body)
		if id, err := uuid.Parse(body.ID); err != nil || id.Version() != 7 {
			t.Errorf("sent id %q, which the server refuses unless it is a version 7 UUID", body.ID)
		}
		sentFor[body.VideoID] = append(sentFor[body.VideoID], body.ID)
	}
	for videoID, ids := range sentFor {
		if len(slices.Compact(slices.Clone(ids))) != 1 {
			t.Errorf("sent %s under %v, want every attempt under one id", videoID, ids)
		}
	}
	for _, never := range []string{"bbbbbbbbbbb", "ooooooooooo"} {
		if ids, sent := sentFor[never]; sent {
			t.Errorf("counted %s as heard, under %v", never, ids)
		}
	}
	if len(sentFor["aaaaaaaaaaa"]) != 2 || len(sentFor["xxxxxxxxxxx"]) < 2 {
		t.Errorf("sent the refused plays %d and %d times, want each sent again", len(sentFor["aaaaaaaaaaa"]), len(sentFor["xxxxxxxxxxx"]))
	}
	if len(sentFor["zzzzzzzzzzz"]) != 1 {
		t.Errorf("sent the deleted play %d times, want once and then settled", len(sentFor["zzzzzzzzzzz"]))
	}

	// What went unsent is named with the command that records it.
	var said bytes.Buffer
	told := &cobra.Command{}
	told.SetErr(&said)
	reportPlays(told, result)
	if !strings.Contains(said.String(), "Recorded 5 plays") || !strings.Contains(said.String(), "`ypl plays add yyyyyyyyyyy`") {
		t.Errorf("reported %q, want the five stored and the command recording the sixth", said.String())
	}
}

// What the command is responsible for is the command line it builds, so that is
// what is read back: the socket flag, the pass-through arguments and the URLs
// in the playlist's order, with the unavailable ones left out.
func TestPlayHandsMpvTheUrlsItCanServeAndNothingElse(t *testing.T) {
	held := `{"id": "PLA", "title": "Sunday Morning", "privacy": "private", "item_count": 5,
		"enriched_count": 3, "unavailable_count": 2, "synced_ts": null, "items": [
			{"position": 1, "video": {"id": "aaaaaaaaaaa", "title": "A", "channel_title": "One", "duration_seconds": 60, "is_unavailable": false}},
			{"position": 2, "video": {"id": "bbbbbbbbbbb", "title": "B", "channel_title": "One", "duration_seconds": 60, "is_unavailable": true}},
			{"position": 3, "video": {"id": "ccccccccccc", "title": "C", "channel_title": "One", "duration_seconds": 60, "is_unavailable": false}},
			{"position": 4, "video": {"id": "ddddddddddd", "title": "D", "channel_title": "One", "duration_seconds": 60, "is_unavailable": true}},
			{"position": 5, "video": {"id": "eeeeeeeeeee", "title": "E", "channel_title": "One", "duration_seconds": 60, "is_unavailable": false}}]}`
	serve := serves(map[string]string{"/api/v1/playlists/Sunday Morning": held})

	f := newFixture(t, serve)
	shortStateDir(t)
	argv := stubMpv(t, 0)
	got := f.run("play", "Sunday Morning", "--audio", "--mpv", "--start=30")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	handed := argvOf(t, argv)
	want := []string{
		"--input-ipc-server=" + mpv.SocketPath(),
		"--start=30",
		"--no-video",
		"https://www.youtube.com/watch?v=aaaaaaaaaaa",
		"https://www.youtube.com/watch?v=ccccccccccc",
		"https://www.youtube.com/watch?v=eeeeeeeeeee",
	}
	if !slices.Equal(handed, want) {
		t.Errorf("handed mpv %v,\nwant %v", handed, want)
	}
	if !strings.Contains(got.err, "2 videos") {
		t.Errorf("said %q, want the count YouTube will not serve", got.err)
	}

	// The limit is a ceiling on what plays, so it is applied before the
	// unavailable ones are counted — otherwise the count reports videos dropped
	// from a part of the playlist that was never going to play.
	f = newFixture(t, serve)
	argv = stubMpv(t, 0)
	if got := f.run("play", "Sunday Morning", "--limit", "2"); got.code != 0 {
		t.Fatalf("under a limit, exited %d: %s%s", got.code, got.out, got.err)
	}
	handed = argvOf(t, argv)
	if played := handed[len(handed)-2:]; !slices.Equal(played, []string{
		"https://www.youtube.com/watch?v=aaaaaaaaaaa",
		"https://www.youtube.com/watch?v=ccccccccccc",
	}) {
		t.Errorf("under a limit of 2 played %v, want the first two it can serve", played)
	}

	// `--limit 0` is a request for nothing on every other verb of this binary,
	// so it is a request for nothing here. Nothing runs, and asking for none is
	// not a failure.
	f = newFixture(t, serve)
	argv = stubMpv(t, 0)
	none := f.run("play", "Sunday Morning", "--limit", "0")
	if none.code != 0 {
		t.Errorf("--limit 0 exited %d, want 0", none.code)
	}
	if _, err := os.ReadFile(argv); err == nil {
		t.Error("--limit 0 started a player, and it asked for no videos")
	}

	// A machine without mpv is told so before anything reaches the server, so
	// one with neither mpv nor a reachable server is told the right one.
	f = newFixture(t, serve)
	t.Setenv("PATH", t.TempDir())
	missing := f.run("play", "Sunday Morning")
	if len(f.sent) != 0 || !strings.Contains(missing.err, "install") {
		t.Errorf("without mpv asked the server %d times and said %q, want none and what to install", len(f.sent), missing.err)
	}

	// mpv spends 2 on a file it cannot open and this binary spends 2 on an
	// invocation it would not accept, so mpv's status is said rather than
	// returned.
	f = newFixture(t, serve)
	stubMpv(t, 2)
	failed := f.run("play", "Sunday Morning")
	if failed.code != 1 {
		t.Errorf("a failed player exited %d, want 1 rather than mpv's own 2", failed.code)
	}
	if !strings.Contains(failed.err, "2") {
		t.Errorf("said %q, want it to name what mpv returned", failed.err)
	}

	// Ctrl-C reaches this process as well as mpv. It is caught for as long as
	// the player runs, so what comes after the player still happens, and the
	// command answers with the shell's code for an interrupt. Uncaught, it
	// ends the test binary here.
	f = newFixture(t, serve)
	still := t.TempDir()
	if err := os.WriteFile(filepath.Join(still, "mpv"), []byte("#!/bin/sh\n/bin/sleep 1\n"), 0o700); err != nil {
		t.Fatalf("write the stub: %v", err)
	}
	t.Setenv("PATH", still+string(os.PathListSeparator)+os.Getenv("PATH"))
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	if stopped := f.run("play", "Sunday Morning"); stopped.code != 130 {
		t.Errorf("an interrupted playback exited %d, want 130: %s", stopped.code, stopped.err)
	}

	// With no playlist the library plays in the order the server draws it,
	// asked for as much as one draw holds.
	f = newFixture(t, serves(map[string]string{"/api/v1/suggestions": `[
		{"id": "ccccccccccc", "title": "C", "channel_title": "One", "duration_seconds": 60, "play_count": 0, "last_played_ts": null},
		{"id": "aaaaaaaaaaa", "title": "A", "channel_title": "One", "duration_seconds": 60, "play_count": 1, "last_played_ts": "2026-01-01T00:00:00Z"}]`}))
	argv = stubMpv(t, 0)
	if got := f.run("play"); got.code != 0 {
		t.Fatalf("bare, exited %d: %s%s", got.code, got.out, got.err)
	}
	handed = argvOf(t, argv)
	if played := handed[len(handed)-2:]; !slices.Equal(played, []string{
		"https://www.youtube.com/watch?v=ccccccccccc",
		"https://www.youtube.com/watch?v=aaaaaaaaaaa",
	}) {
		t.Errorf("bare played %v, want the draw in the server's order", played)
	}
	if asked := f.lastAsked().Query().Get("limit"); asked != strconv.Itoa(api.MaxSuggestions) {
		t.Errorf("drew %s, want as much as one draw holds", asked)
	}
}

// argvOf is what the stub was handed, one argument a line.
func argvOf(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stub wrote no arguments: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}
