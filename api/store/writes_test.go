package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/youtube"
)

// sent is 22:00:00 Pacific on 2026-09-17, which is already the 18th in UTC.
var sent = time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)

func TestOpenSeedsTheYouTubeWriteAndPlaylistVocabularies(t *testing.T) {
	st, _ := open(t)
	for table, want := range map[string]int{
		"youtube_write_outcomes": len(youtubeWriteOutcomes),
		"youtube_write_methods":  len(youtubeWriteMethods),
		"playlist_sorts":         len(playlistSorts),
		"base_states":            len(baseStates),
	} {
		if n := count(t, st, table); n != want {
			t.Errorf("%s = %d rows, want %d", table, n, want)
		}
	}
}

// begin records w as pending.
func begin(st *Store, w Write) (int64, error) {
	ctx := context.Background()
	var id int64
	err := st.InTx(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.BeginWrite(ctx, w)
		return err
	})
	return id, err
}

// settle records a write of method to playlist, sent at sent, settled as
// outcome at settledAt.
func settle(t *testing.T, st *Store, method, playlist, outcome string, settledAt time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := begin(st, Write{Method: method, PlaylistID: playlist, SentAt: sent})
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	var cause error
	if outcome != WriteApplied {
		cause = errors.New("YouTube said no")
	}
	err = st.InTx(ctx, func(tx *Tx) error {
		return tx.SettleWrite(ctx, Settlement{WriteID: id, PlaylistID: playlist, Outcome: outcome, SettledAt: settledAt, Requests: 1, Units: 50, Err: cause})
	})
	if err != nil {
		t.Fatalf("SettleWrite as %s: %v", outcome, err)
	}
}

func TestAWriteIsRecordedPendingAndSettlesOnce(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	id, err := begin(st, Write{Method: youtube.MethodPlaylistsInsert, SentAt: sent})
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	pending, err := st.Queries.GetYouTubeWrite(ctx, id)
	if err != nil || pending.Outcome != WritePending || pending.PlaylistID.Valid || pending.SentTs != "2026-09-18T05:00:00Z" || pending.QuotaDate != "2026-09-17" || pending.Units.Valid {
		t.Fatalf("pending write = %+v, %v, want it pending on the Pacific date with no playlist or cost", pending, err)
	}

	settlement := Settlement{WriteID: id, PlaylistID: "PLnew", Outcome: WriteApplied, SettledAt: sent.Add(time.Second), Requests: 2, Units: 100}
	if err := st.InTx(ctx, func(tx *Tx) error { return tx.SettleWrite(ctx, settlement) }); err != nil {
		t.Fatalf("SettleWrite: %v", err)
	}
	settled, err := st.Queries.GetYouTubeWrite(ctx, id)
	if err != nil || settled.Outcome != WriteApplied || settled.PlaylistID.String != "PLnew" || settled.SettledTs.String != "2026-09-18T05:00:01Z" || settled.Requests.Int64 != 2 || settled.Units.Int64 != 100 || settled.Error.Valid {
		t.Fatalf("settled write = %+v, %v, want it applied to PLnew at 05:00:01 UTC costing 2 requests and 100 units", settled, err)
	}

	if err := st.InTx(ctx, func(tx *Tx) error { return tx.SettleWrite(ctx, settlement) }); err == nil {
		t.Fatal("a second SettleWrite succeeded, want it refused")
	}
}

// An insert names its video and position and learns its item when it settles.
// A move names its item as it begins, and settling keeps it.
func TestAnItemWriteRecordsWhatItNamed(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	insert, err := begin(st, Write{Method: youtube.MethodPlaylistItemsInsert, PlaylistID: "PLA", VideoID: "a", Position: sql.NullInt64{Int64: 2, Valid: true}, SentAt: sent})
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	move, err := begin(st, Write{Method: youtube.MethodPlaylistItemsUpdate, PlaylistID: "PLA", ItemID: "i1", Position: sql.NullInt64{Int64: 0, Valid: true}, SentAt: sent})
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	err = st.InTx(ctx, func(tx *Tx) error {
		if err := tx.SettleWrite(ctx, Settlement{WriteID: insert, PlaylistID: "PLA", ItemID: "i9", Outcome: WriteApplied, SettledAt: sent, Requests: 1, Units: 50}); err != nil {
			return err
		}
		return tx.SettleWrite(ctx, Settlement{WriteID: move, PlaylistID: "PLA", Outcome: WriteApplied, SettledAt: sent, Requests: 1, Units: 50})
	})
	if err != nil {
		t.Fatalf("SettleWrite: %v", err)
	}
	if row, err := st.Queries.GetYouTubeWrite(ctx, insert); err != nil || row.VideoID.String != "a" || row.Position.Int64 != 2 || row.ItemID.String != "i9" {
		t.Errorf("insert = %+v, %v, want video a at 2 made as item i9", row, err)
	}
	if row, err := st.Queries.GetYouTubeWrite(ctx, move); err != nil || row.ItemID.String != "i1" || !row.Position.Valid || row.Position.Int64 != 0 || row.VideoID.Valid {
		t.Errorf("move = %+v, %v, want item i1 to 0 and no video", row, err)
	}
}

// Only a write that did not apply carries why, so an outcome and its error
// cannot disagree.
func TestAWriteSettledWithAnErrorItsOutcomeContradictsIsRefused(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	cases := map[string]Settlement{
		"applied with an error":   {Outcome: WriteApplied, Err: errors.New("no")},
		"refused without a cause": {Outcome: WriteRefused},
		"settled as pending":      {Outcome: WritePending},
	}
	for name, settlement := range cases {
		id, err := begin(st, Write{Method: youtube.MethodPlaylistsDelete, PlaylistID: "PLA", SentAt: sent})
		if err != nil {
			t.Fatalf("BeginWrite: %v", err)
		}
		settlement.WriteID, settlement.SettledAt = id, sent
		if err := st.InTx(ctx, func(tx *Tx) error { return tx.SettleWrite(ctx, settlement) }); err == nil {
			t.Errorf("%s: SettleWrite succeeded, want the row refused", name)
		}
	}
	if _, err := begin(st, Write{Method: "playlists.rename", PlaylistID: "PLA", SentAt: sent}); err == nil {
		t.Error("a write of a method the vocabulary does not hold was recorded")
	}
}

func TestWriteNewerThanReadFindsOnlyAnAnsweredWriteWithinTheLag(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	answered := sent.Add(time.Second)
	settle(t, st, youtube.MethodPlaylistsUpdate, "PLA", WriteApplied, answered)
	settle(t, st, youtube.MethodPlaylistsDelete, "PLB", WriteAbsent, answered)
	settle(t, st, youtube.MethodPlaylistsUpdate, "PLC", WriteRefused, answered)
	settle(t, st, youtube.MethodPlaylistsUpdate, "PLD", WriteUnanswered, answered)
	if _, err := begin(st, Write{Method: youtube.MethodPlaylistsUpdate, PlaylistID: "PLE", SentAt: sent}); err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	settle(t, st, youtube.MethodPlaylistsUpdate, "PLF", WriteApplied, answered)
	settle(t, st, youtube.MethodPlaylistsDelete, "PLF", WriteApplied, answered.Add(time.Second))

	cases := []struct {
		playlist string
		readAt   time.Time
		method   string
	}{
		{"PLA", answered.Add(-time.Hour), youtube.MethodPlaylistsUpdate},
		{"PLA", answered.Add(youtube.ReadLag - time.Second), youtube.MethodPlaylistsUpdate},
		{"PLA", answered.Add(youtube.ReadLag), ""},
		{"PLB", answered, youtube.MethodPlaylistsDelete},
		{"PLC", answered, ""},
		{"PLD", answered, ""},
		{"PLE", answered, ""},
		{"PLF", answered, youtube.MethodPlaylistsDelete},
		{"PLZ", answered, ""},
	}
	for _, c := range cases {
		method, ok, err := WriteNewerThanRead(ctx, st.Queries, c.playlist, c.readAt)
		if err != nil || method != c.method || ok != (c.method != "") {
			t.Errorf("WriteNewerThanRead(%s, read %s) = %q, %v, %v, want %q", c.playlist, c.readAt.Sub(answered), method, ok, err, c.method)
		}
	}
}

// An item write whatever its outcome, settled or still pending, holds back a
// read within the lag of it. A playlist write does not.
func TestItemWritesSinceSeesEveryItemWriteWithinTheLag(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	answered := sent.Add(time.Second)
	settle(t, st, youtube.MethodPlaylistItemsInsert, "PLA", WriteRefused, answered)
	settle(t, st, youtube.MethodPlaylistsUpdate, "PLB", WriteApplied, answered)
	if _, err := begin(st, Write{Method: youtube.MethodPlaylistItemsDelete, PlaylistID: "PLC", ItemID: "i1", SentAt: sent}); err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}

	cases := []struct {
		playlist string
		readAt   time.Time
		want     bool
	}{
		{"PLA", answered.Add(youtube.ReadLag - time.Second), true},
		{"PLA", answered.Add(youtube.ReadLag), false},
		{"PLB", answered, false},
		{"PLC", sent.Add(youtube.ReadLag - time.Second), true},
		{"PLC", sent.Add(youtube.ReadLag), false},
		{"PLZ", answered, false},
	}
	for _, c := range cases {
		if got, err := ItemWritesSince(ctx, st.Queries, c.playlist, c.readAt); err != nil || got != c.want {
			t.Errorf("ItemWritesSince(%s, read %s after sending) = %v, %v, want %v", c.playlist, c.readAt.Sub(sent), got, err, c.want)
		}
	}
}
