package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/ytdlp"
)

// fakeDoor answers reads through the door with transcript, or with err, and
// records each read's class and the most audio it allowed. Its audio is the
// file audio names. Under slow, a read of captions runs until its context
// ends.
type fakeDoor struct {
	transcript ytdlp.Transcript
	audio      string
	err        error
	pause      ytdlp.Pause
	slow       bool
	classes    []ytdlp.Class
	most       []int64
}

func (f *fakeDoor) Captions(ctx context.Context, class ytdlp.Class, id string) (ytdlp.Captions, error) {
	f.classes = append(f.classes, class)
	if f.slow {
		<-ctx.Done()
		return ytdlp.Captions{}, ctx.Err()
	}
	return ytdlp.Captions{VideoID: id, Segments: []ytdlp.Segment{}}, f.err
}

func (f *fakeDoor) Transcript(_ context.Context, class ytdlp.Class, _, _ string) (ytdlp.Transcript, error) {
	f.classes = append(f.classes, class)
	return f.transcript, f.err
}

func (f *fakeDoor) Audio(_ context.Context, class ytdlp.Class, _ string, maxBytes int64) (*ytdlp.Audio, error) {
	f.classes, f.most = append(f.classes, class), append(f.most, maxBytes)
	if f.err != nil {
		return nil, f.err
	}
	info, err := os.Stat(f.audio)
	if err != nil {
		return nil, err
	}
	return &ytdlp.Audio{Path: f.audio, Ext: "webm", Size: info.Size()}, nil
}

func (f *fakeDoor) PausedUntil() (ytdlp.Pause, bool) {
	return f.pause, !f.pause.Until.IsZero()
}

func (f *fakeDoor) Run(_ context.Context, class ytdlp.Class, _ ytdlp.RunArguments, _ int64) (*ytdlp.Run, error) {
	f.classes = append(f.classes, class)
	return nil, f.err
}

// Lease is held for LeaseHold from arrival.
func (f *fakeDoor) Lease(_ context.Context, class ytdlp.Class) (time.Time, error) {
	f.classes = append(f.classes, class)
	return arrival.Add(ytdlp.LeaseHold), f.err
}

func (f *fakeDoor) Pace() ytdlp.Pace { return ytdlp.DefaultPace }

// While the door's pause holds, a person's read and a service's are each
// answered 503 with a Retry-After of when the pause ends.
func TestEveryClassIsTurnedAwayWithARetryAfterWhileTheDoorIsPaused(t *testing.T) {
	f := newFixture(t)
	until := arrival.Add(time.Hour)
	door := &fakeDoor{err: &ytdlp.PausedError{Pause: ytdlp.Pause{At: arrival, Until: until, Cause: ytdlp.PausedRefused}}}
	f.h.ReadVideosWith(door, "ypl.example.com")

	for _, rec := range []*http.Response{
		f.get("/api/v1/youtube/videos/Wr1tt3nVid0/captions").Result(),
		f.doAs(ingestion, http.MethodGet, "/api/v1/youtube/videos/Wr1tt3nVid0/transcript?lang=en", "").Result(),
	} {
		if rec.StatusCode != http.StatusServiceUnavailable || rec.Header.Get("Retry-After") != "3600" {
			t.Errorf("a read while the door is paused answered %d with Retry-After %q, want 503 and 3600", rec.StatusCode, rec.Header.Get("Retry-After"))
		}
	}
	if len(door.classes) != 2 || door.classes[0] != ytdlp.Person || door.classes[1] != ytdlp.Service {
		t.Fatalf("the door saw classes %v, want a person's read and then a service's", door.classes)
	}
}

// Audio streams whole to a request that names the server's own address, and is
// capped at 100 MB for one that names its public address.
func TestAudioIsCappedOnlyThroughThePublicAddress(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "audio.webm")
	if err := os.WriteFile(path, []byte("written audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	door := &fakeDoor{audio: path}
	f.h.ReadVideosWith(door, "ypl.example.com")

	for _, host := range []string{"ypl", "YPL.example.com:443"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+"/api/v1/youtube/videos/Wr1tt3nVid0/audio", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "written audio" || rec.Header().Get("Content-Type") != "audio/webm" {
			t.Fatalf("audio through %s answered %d %q as %q, want the file as audio/webm", host, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	if len(door.most) != 2 || door.most[0] != 0 || door.most[1] != maxPublicAnswer {
		t.Fatalf("the reads allowed %v bytes, want no bound and then %d", door.most, maxPublicAnswer)
	}
}

// ypl transcript <id> english, or a service sending lang=pt_BR, is told its
// language is no language, before any read of YouTube.
func TestATranscriptInAMalformedLanguageIsTheCallersMistake(t *testing.T) {
	f := newFixture(t)
	door := &fakeDoor{}
	f.h.ReadVideosWith(door, "ypl.example.com")
	for _, lang := range []string{"english", "pt_BR"} {
		refused(t, f.doAs(ingestion, http.MethodGet, "/api/v1/youtube/videos/Wr1tt3nVid0/transcript?lang="+lang, ""), http.StatusBadRequest, "invalid_parameter")
	}
	if len(door.classes) != 0 {
		t.Fatalf("the door saw %d reads, want none", len(door.classes))
	}
}

// A read through the public address whose wait at the door and read run past
// the budget answers the server's own 503 with a Retry-After, before the proxy
// in front of that address gives up on it.
func TestAReadThroughThePublicAddressPastItsBudgetAnswersBusy(t *testing.T) {
	f := newFixture(t)
	f.h.ReadVideosWith(&fakeDoor{slow: true}, "ypl.example.com")
	f.h.videos.budget = 50 * time.Millisecond
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://ypl.example.com/api/v1/youtube/videos/Wr1tt3nVid0/captions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	refused(t, rec, http.StatusServiceUnavailable, "youtube_reads_busy")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("the read past its budget carries no Retry-After")
	}
}
