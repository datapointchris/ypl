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
// as it is made. A method named in failures returns that error, and one named in
// ignored returns nil without applying anything.
type stubChannel struct {
	playlists []youtube.Playlist
	items     map[youtube.PlaylistID][]youtube.Item
	failures  map[string]error
	ignored   map[string]bool
	// createLostAnswer makes CreatePlaylist apply the create and then return
	// its failure, as a create whose answer never arrived does.
	createLostAnswer bool
	// insertAppends makes InsertItem ignore its position and append.
	insertAppends bool
	// updateDropsDescription makes UpdatePlaylist clear the description.
	updateDropsDescription bool
	// deleteFailures is the error DeletePlaylist returns for a playlist, before
	// deleting anything.
	deleteFailures map[youtube.PlaylistID]error
	// onSourceRead runs when the check reads the source playlist's items.
	onSourceRead func()
	// calls is each call made, with the arguments that tell the calls apart.
	calls []string
	// createContextErr and deleteContextErrs are the errors of the contexts the
	// create and each deletion were given.
	createContextErr  error
	deleteContextErrs []error
	added             int
}

func newStubChannel(source []youtube.Item) *stubChannel {
	return &stubChannel{
		playlists: []youtube.Playlist{{ID: "PLsource", Title: "Source", Privacy: "private"}},
		items:     map[youtube.PlaylistID][]youtube.Item{"PLsource": source},
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

func (s *stubChannel) Items(_ context.Context, playlistID youtube.PlaylistID) ([]youtube.Item, error) {
	if _, err := s.call("Items", playlistID); err != nil {
		return nil, err
	}
	if playlistID == "PLsource" && s.onSourceRead != nil {
		s.onSourceRead()
	}
	return slices.Clone(s.items[playlistID]), nil
}

func (s *stubChannel) CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.Playlist, error) {
	s.createContextErr = ctx.Err()
	_, err := s.call("CreatePlaylist")
	if err != nil && !s.createLostAnswer {
		return youtube.Playlist{}, err
	}
	created := youtube.Playlist{ID: "PLcheck", Title: details.Title, Description: details.Description, Privacy: "private"}
	s.playlists = append(s.playlists, created)
	s.items["PLcheck"] = nil
	if err != nil {
		return youtube.Playlist{}, err
	}
	return created, nil
}

func (s *stubChannel) UpdatePlaylist(_ context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) (youtube.PlaylistDetails, error) {
	if ignored, err := s.call("UpdatePlaylist", id); err != nil || ignored {
		return youtube.PlaylistDetails{}, err
	}
	index := slices.IndexFunc(s.playlists, func(p youtube.Playlist) bool { return p.ID == id })
	s.playlists[index].Title, s.playlists[index].Description = details.Title, details.Description
	if s.updateDropsDescription {
		s.playlists[index].Description = ""
	}
	return youtube.PlaylistDetails{Title: s.playlists[index].Title, Description: s.playlists[index].Description}, nil
}

func (s *stubChannel) DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error {
	s.deleteContextErrs = append(s.deleteContextErrs, ctx.Err())
	if _, err := s.call("DeletePlaylist", id); err != nil {
		return err
	}
	if err := s.deleteFailures[id]; err != nil {
		return err
	}
	s.playlists = slices.DeleteFunc(s.playlists, func(p youtube.Playlist) bool { return p.ID == id })
	delete(s.items, id)
	return nil
}

func (s *stubChannel) InsertItem(_ context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error) {
	if _, err := s.call("InsertItem", video, position); err != nil {
		return "", err
	}
	s.added++
	item := youtube.Item{ID: youtube.ItemID(fmt.Sprintf("item-%d", s.added)), PlaylistID: playlist, VideoID: video}
	at := int(position)
	if s.insertAppends {
		at = len(s.items[playlist])
	}
	s.items[playlist] = slices.Insert(s.items[playlist], at, item)
	return item.ID, nil
}

func (s *stubChannel) MoveItem(_ context.Context, item youtube.Item, position int64) error {
	if ignored, err := s.call("MoveItem", item.ID, position); err != nil || ignored {
		return err
	}
	items := slices.DeleteFunc(s.items[item.PlaylistID], func(it youtube.Item) bool { return it.ID == item.ID })
	s.items[item.PlaylistID] = slices.Insert(items, int(position), item)
	return nil
}

func (s *stubChannel) DeleteItem(_ context.Context, id youtube.ItemID) error {
	if ignored, err := s.call("DeleteItem", id); err != nil || ignored {
		return err
	}
	for playlist, items := range s.items {
		s.items[playlist] = slices.DeleteFunc(items, func(it youtube.Item) bool { return it.ID == id })
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

func listed(s *stubChannel, id youtube.PlaylistID) bool {
	return slices.ContainsFunc(s.playlists, func(p youtube.Playlist) bool { return p.ID == id })
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
		"Playlists",
		"CreatePlaylist", "InsertItem first 0", "InsertItem second 0", "UpdatePlaylist PLcheck",
		"Playlists", "Items PLcheck",
		"MoveItem item-1 0",
		"Playlists", "Items PLcheck",
		"DeleteItem item-2",
		"Playlists", "Items PLcheck",
		"DeletePlaylist PLcheck",
	}
	if !slices.Equal(stub.calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", stub.calls, want)
	}
	if settled != 3 {
		t.Fatalf("settled %d times, want once before each read-back", settled)
	}
	rep := decodeReport(t, &out)
	if rep.Playlist == nil || *rep.Playlist != "PLcheck" || !rep.Deleted || len(rep.Swept) != 0 || rep.Error != nil || rep.Requests != int64(len(want)) || rep.Units != 350 {
		t.Fatalf("report %+v, want PLcheck deleted, nothing swept, no error, %d requests and 350 units", rep, len(want))
	}
	if listed(stub, "PLcheck") {
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
	if rep := decodeReport(t, &out); !rep.Deleted || rep.Error == nil || listed(stub, "PLcheck") {
		t.Fatalf("report %+v, want the playlist deleted and the error reported", rep)
	}
}

func TestAnInterruptDuringTheWritesStillDeletesThePlaylist(t *testing.T) {
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
	if slices.ContainsFunc(stub.deleteContextErrs, func(err error) bool { return err != nil }) {
		t.Fatalf("a deletion was given a context that had ended: %v", stub.deleteContextErrs)
	}
	if rep := decodeReport(t, &out); !rep.Deleted || listed(stub, "PLcheck") {
		t.Fatalf("report %+v, want the playlist deleted", rep)
	}
}

// An interrupt that arrives before the create does not cancel it, so the create
// returns the id the deletion needs.
func TestAnInterruptBeforeTheCreateLeavesTheCreateItsAnswer(t *testing.T) {
	stub := newStubChannel(sourceItems())
	ctx, cancel := context.WithCancel(context.Background())
	stub.onSourceRead = cancel
	var out bytes.Buffer

	_ = check(ctx, stub, func(ctx context.Context) error { return ctx.Err() }, &out)
	if stub.createContextErr != nil {
		t.Fatalf("the create was given a context that had ended: %v", stub.createContextErr)
	}
	if rep := decodeReport(t, &out); rep.Playlist == nil || !rep.Deleted || listed(stub, "PLcheck") {
		t.Fatalf("report %+v, want the created playlist deleted", rep)
	}
}

func TestACreateWhoseAnswerIsLostIsSweptAfterIt(t *testing.T) {
	stub := newStubChannel(sourceItems())
	lost := errors.New("connection reset")
	stub.failures["CreatePlaylist"] = lost
	stub.createLostAnswer = true
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, lost) {
		t.Fatalf("check = %v, want the create's error", err)
	}
	rep := decodeReport(t, &out)
	if rep.Playlist != nil || !slices.Equal(rep.Swept, []youtube.PlaylistID{"PLcheck"}) || rep.Error == nil || listed(stub, "PLcheck") {
		t.Fatalf("report %+v, want no playlist, PLcheck swept and the error reported", rep)
	}
}

func TestAPlaylistAnEarlierCheckLeftIsSweptFirst(t *testing.T) {
	stub := newStubChannel(sourceItems())
	stub.playlists = append(stub.playlists,
		youtube.Playlist{ID: "PLleft", Title: renamed, Description: description, Privacy: "private"},
		youtube.Playlist{ID: "PLmine", Title: title, Description: "A playlist of my own", Privacy: "private"},
		youtube.Playlist{ID: "PLpublic", Title: title, Description: description, Privacy: "public"},
	)
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	if rep := decodeReport(t, &out); !slices.Equal(rep.Swept, []youtube.PlaylistID{"PLleft"}) {
		t.Fatalf("swept %v, want PLleft alone", rep.Swept)
	}
	if listed(stub, "PLleft") || !listed(stub, "PLmine") || !listed(stub, "PLpublic") {
		t.Fatalf("playlists %+v, want PLleft gone and PLmine and PLpublic kept", stub.playlists)
	}
}

// A list made seconds after a delete can still show the deleted playlist.
func TestALeftoverYouTubeReportsGoneIsPassedOver(t *testing.T) {
	stub := newStubChannel(sourceItems())
	stub.playlists = append(stub.playlists, youtube.Playlist{ID: "PLgone", Title: title, Description: description, Privacy: "private"})
	stub.deleteFailures = map[youtube.PlaylistID]error{"PLgone": fmt.Errorf("delete playlist PLgone: %w", youtube.ErrPlaylistNotFound)}
	var out bytes.Buffer

	if err := check(context.Background(), stub, noWait, &out); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	if rep := decodeReport(t, &out); len(rep.Swept) != 0 || !rep.Deleted {
		t.Fatalf("report %+v, want nothing swept and the check's playlist deleted", rep)
	}
}

func TestAWriteThatDidNotLandAsWrittenFailsTheReadBack(t *testing.T) {
	cases := map[string]func(*stubChannel){
		"an insert that ignores its position": func(s *stubChannel) { s.insertAppends = true },
		"a move that never lands":             func(s *stubChannel) { s.ignored["MoveItem"] = true },
		"a rename that never lands":           func(s *stubChannel) { s.ignored["UpdatePlaylist"] = true },
		"a rename that drops the description": func(s *stubChannel) { s.updateDropsDescription = true },
		"an item delete that never lands":     func(s *stubChannel) { s.ignored["DeleteItem"] = true },
	}
	for name, breakWrite := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newStubChannel(sourceItems())
			breakWrite(stub)
			var out bytes.Buffer

			if err := check(context.Background(), stub, noWait, &out); !errors.Is(err, ErrReadBack) {
				t.Fatalf("check = %v, want ErrReadBack", err)
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
