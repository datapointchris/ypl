package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/youtube"
)

// stubChannel holds playlists and their items in memory and applies each write
// as it is made. A method named in failures returns that error, and one named
// in ignored returns nil without applying anything.
type stubChannel struct {
	playlists []youtube.Playlist
	items     map[string][]youtube.Item
	failures  map[string]error
	ignored   map[string]bool
	// calls is each call made, with the arguments that tell the calls apart.
	calls []string
	// deleteContextErr is the error of the context DeletePlaylist was given.
	deleteContextErr error
	added            int
}

func newStubChannel(source []youtube.Item) *stubChannel {
	return &stubChannel{
		playlists: []youtube.Playlist{{ID: "PLsource", Title: "Source"}},
		items:     map[string][]youtube.Item{"PLsource": source},
		failures:  map[string]error{},
		ignored:   map[string]bool{},
	}
}

// call records the call name made with args, and reports whether the call is to
// be ignored and the error it is to return.
func (s *stubChannel) call(name string, args ...any) (bool, error) {
	s.calls = append(s.calls, strings.TrimSuffix(fmt.Sprintln(append([]any{name}, args...)...), "\n"))
	return s.ignored[name], s.failures[name]
}

func (s *stubChannel) Playlists(context.Context) ([]youtube.Playlist, error) {
	if _, err := s.call("Playlists"); err != nil {
		return nil, err
	}
	return slices.Clone(s.playlists), nil
}

func (s *stubChannel) Items(_ context.Context, playlistID string) ([]youtube.Item, error) {
	if _, err := s.call("Items", playlistID); err != nil {
		return nil, err
	}
	return slices.Clone(s.items[playlistID]), nil
}

func (s *stubChannel) CreatePlaylist(_ context.Context, title, description string) (youtube.Playlist, error) {
	if _, err := s.call("CreatePlaylist"); err != nil {
		return youtube.Playlist{}, err
	}
	created := youtube.Playlist{ID: "PLcheck", Title: title, Description: description, Privacy: "private"}
	s.playlists = append(s.playlists, created)
	s.items[created.ID] = nil
	return created, nil
}

func (s *stubChannel) UpdatePlaylist(_ context.Context, playlist youtube.Playlist) error {
	if ignored, err := s.call("UpdatePlaylist"); err != nil || ignored {
		return err
	}
	index := slices.IndexFunc(s.playlists, func(p youtube.Playlist) bool { return p.ID == playlist.ID })
	s.playlists[index].Title, s.playlists[index].Description = playlist.Title, playlist.Description
	return nil
}

func (s *stubChannel) DeletePlaylist(ctx context.Context, playlistID string) error {
	s.deleteContextErr = ctx.Err()
	if _, err := s.call("DeletePlaylist"); err != nil {
		return err
	}
	s.playlists = slices.DeleteFunc(s.playlists, func(p youtube.Playlist) bool { return p.ID == playlistID })
	delete(s.items, playlistID)
	return nil
}

func (s *stubChannel) InsertItem(_ context.Context, playlistID, videoID string, position int64) (youtube.Item, error) {
	if _, err := s.call("InsertItem", videoID, position); err != nil {
		return youtube.Item{}, err
	}
	s.added++
	item := youtube.Item{ID: fmt.Sprintf("item-%d", s.added), PlaylistID: playlistID, VideoID: videoID}
	s.items[playlistID] = slices.Insert(s.items[playlistID], int(position), item)
	return item, nil
}

func (s *stubChannel) MoveItem(_ context.Context, item youtube.Item, position int64) error {
	if ignored, err := s.call("MoveItem", item.ID, position); err != nil || ignored {
		return err
	}
	items := slices.DeleteFunc(s.items[item.PlaylistID], func(it youtube.Item) bool { return it.ID == item.ID })
	s.items[item.PlaylistID] = slices.Insert(items, int(position), item)
	return nil
}

func (s *stubChannel) DeleteItem(_ context.Context, itemID string) error {
	if ignored, err := s.call("DeleteItem", itemID); err != nil || ignored {
		return err
	}
	for id, items := range s.items {
		s.items[id] = slices.DeleteFunc(items, func(it youtube.Item) bool { return it.ID == itemID })
	}
	return nil
}

func (s *stubChannel) Requests() int64 { return int64(len(s.calls)) }

func (s *stubChannel) Units() int64 { return 7 * 50 }

// sourceItems is a playlist whose available videos, in order, are first and
// second.
func sourceItems() []youtube.Item {
	return []youtube.Item{
		{ID: "s0", VideoID: "private", Unavailable: true},
		{ID: "s1", VideoID: "first"},
		{ID: "s2", VideoID: "first"},
		{ID: "s3", VideoID: "second"},
	}
}

func noWait(context.Context) error { return nil }

func noCredentials(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	t.Setenv("YOUTUBE_REFRESH_TOKEN", "")
}

func decodeReport(t *testing.T, out *bytes.Buffer) report {
	t.Helper()
	var rep report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode report %q: %v", out.String(), err)
	}
	return rep
}

func TestHelpWritesNothing(t *testing.T) {
	noCredentials(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &out, io.Discard); err != nil {
		t.Fatalf("run -h = %v, want nil before any credential is read", err)
	}
	if out.Len() != 0 {
		t.Fatalf("run -h wrote a report: %s", out.String())
	}
}

func TestAnythingButNoArgumentsIsAUsageError(t *testing.T) {
	noCredentials(t)
	for _, args := range [][]string{{"-bogus"}, {"extra"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, ErrUsage) {
			t.Errorf("run %q = %v, want ErrUsage before any credential is read", args, err)
		}
	}
}

func TestMissingCredentialsStopTheCheckBeforeAnyRequest(t *testing.T) {
	noCredentials(t)
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out, io.Discard); !errors.Is(err, youtube.ErrMissingCredentials) {
		t.Fatalf("run = %v, want ErrMissingCredentials", err)
	}
	if out.Len() != 0 {
		t.Fatalf("run wrote a report: %s", out.String())
	}
}

func TestACheckThatReadsBackAsWrittenPassesAndDeletesItsPlaylist(t *testing.T) {
	stub := newStubChannel(sourceItems())
	settled := 0
	var out bytes.Buffer

	err := check(context.Background(), stub, func(context.Context) error { settled++; return nil }, &out)
	if err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	want := []string{
		"Playlists", "Items PLsource",
		"CreatePlaylist", "InsertItem first 0", "InsertItem second 0", "MoveItem item-1 0", "UpdatePlaylist",
		"Playlists", "Items PLcheck",
		"DeleteItem item-2",
		"Playlists", "Items PLcheck",
		"DeletePlaylist",
	}
	if !slices.Equal(stub.calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", stub.calls, want)
	}
	if settled != 2 {
		t.Fatalf("settled %d times, want once before each read-back", settled)
	}
	rep := decodeReport(t, &out)
	if rep.Playlist == nil || *rep.Playlist != "PLcheck" || !rep.Deleted || rep.Error != nil || rep.Requests != int64(len(want)) || rep.Units != 350 {
		t.Fatalf("report %+v, want PLcheck deleted with no error, %d requests and 350 units", rep, len(want))
	}
	if slices.ContainsFunc(stub.playlists, func(p youtube.Playlist) bool { return p.ID == "PLcheck" }) {
		t.Fatal("the check's playlist is still on the channel")
	}
}

func TestAFailedWriteStillDeletesThePlaylistAndFailsTheCheck(t *testing.T) {
	stub := newStubChannel(sourceItems())
	refused := errors.New("refused")
	stub.failures["MoveItem"] = refused
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, refused) {
		t.Fatalf("check = %v, want the move's error", err)
	}
	rep := decodeReport(t, &out)
	if !rep.Deleted || rep.Error == nil {
		t.Fatalf("report %+v, want the playlist deleted and the error reported", rep)
	}
}

func TestAnInterruptedCheckStillDeletesItsPlaylist(t *testing.T) {
	stub := newStubChannel(sourceItems())
	ctx, cancel := context.WithCancel(context.Background())
	interrupt := func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}
	var out bytes.Buffer

	if err := check(ctx, stub, interrupt, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("check = %v, want context.Canceled", err)
	}
	if stub.deleteContextErr != nil {
		t.Fatalf("DeletePlaylist was given a context that had ended: %v", stub.deleteContextErr)
	}
	if rep := decodeReport(t, &out); !rep.Deleted {
		t.Fatalf("report %+v, want the playlist deleted", rep)
	}
}

func TestAWriteThatDidNotLandFailsTheReadBack(t *testing.T) {
	for _, write := range []string{"MoveItem", "UpdatePlaylist", "DeleteItem"} {
		t.Run(write, func(t *testing.T) {
			stub := newStubChannel(sourceItems())
			stub.ignored[write] = true
			var out bytes.Buffer

			if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, ErrReadBack) {
				t.Fatalf("check with %s not applied = %v, want ErrReadBack", write, err)
			}
			if rep := decodeReport(t, &out); !rep.Deleted {
				t.Fatalf("report %+v, want the playlist deleted", rep)
			}
		})
	}
}

func TestAPlaylistLeftOnTheChannelFailsTheCheck(t *testing.T) {
	stub := newStubChannel(sourceItems())
	stub.failures["DeletePlaylist"] = errors.New("refused")
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, ErrNotDeleted) {
		t.Fatalf("check = %v, want ErrNotDeleted", err)
	}
	if rep := decodeReport(t, &out); rep.Deleted || rep.Error == nil {
		t.Fatalf("report %+v, want the playlist not deleted and the error reported", rep)
	}
}

func TestAChannelWithoutTwoAvailableVideosCreatesNothing(t *testing.T) {
	stub := newStubChannel([]youtube.Item{{ID: "s0", VideoID: "only"}, {ID: "s1", VideoID: "private", Unavailable: true}})
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, ErrNoVideos) {
		t.Fatalf("check = %v, want ErrNoVideos", err)
	}
	if slices.Contains(stub.calls, "CreatePlaylist") || out.Len() != 0 {
		t.Fatalf("calls %q and output %q, want no playlist created and no report", stub.calls, out.String())
	}
}
