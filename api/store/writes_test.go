package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/youtube"
)

// sent is 22:00:00 Pacific on 2026-09-17, which is already the 18th in UTC.
var sent = time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)

func TestOpenSeedsTheYouTubeWriteVocabularies(t *testing.T) {
	st, _ := open(t)
	if n := count(t, st, "youtube_write_outcomes"); n != len(youtubeWriteOutcomes) {
		t.Errorf("write outcomes = %d, want %d", n, len(youtubeWriteOutcomes))
	}
	if n := count(t, st, "youtube_write_methods"); n != len(youtubeWriteMethods) {
		t.Errorf("write methods = %d, want %d", n, len(youtubeWriteMethods))
	}
}

// settle records a write of method to playlist, sent at sent, settled as
// outcome at settledAt.
func settle(t *testing.T, st *Store, method, playlist, outcome string, settledAt time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := st.BeginWrite(ctx, method, playlist, sent)
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

	id, err := st.BeginWrite(ctx, youtube.MethodPlaylistsInsert, "", sent)
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
		id, err := st.BeginWrite(ctx, youtube.MethodPlaylistsDelete, "PLA", sent)
		if err != nil {
			t.Fatalf("BeginWrite: %v", err)
		}
		settlement.WriteID, settlement.SettledAt = id, sent
		if err := st.InTx(ctx, func(tx *Tx) error { return tx.SettleWrite(ctx, settlement) }); err == nil {
			t.Errorf("%s: SettleWrite succeeded, want the row refused", name)
		}
	}
	if _, err := st.BeginWrite(ctx, "playlists.rename", "PLA", sent); err == nil {
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
	if _, err := st.BeginWrite(ctx, youtube.MethodPlaylistsUpdate, "PLE", sent); err != nil {
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
