package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

const (
	// youtubeTimeout bounds one push write's request to YouTube, with the
	// pauses and attempts of an insert YouTube aborts.
	youtubeTimeout = 15 * time.Second
	// recordTimeout bounds each store step of a push write. SQLite lets a step
	// wait up to 5 seconds for the write lock.
	recordTimeout = 5 * time.Second
)

// WriteDuration is the longest a push write runs once it has begun: its record,
// its request, and the settlement that stores YouTube's answer. A run checks its
// context before each write, and none begins once it has ended.
const WriteDuration = recordTimeout + youtubeTimeout + recordTimeout

// push makes the writes that bring YouTube to the server's order of one merged
// playlist, one at a time. It stops at the first write that does not do what the
// plan expects, since the positions of the writes after it assume it did, and
// the next run plans again from what YouTube then holds. It stops too once the
// playlist's order changes after the merge, or the API deletes the playlist.
//
// A playlist whose push YouTube refused on this Pacific day, with nothing
// changed since, is recorded as held and not pushed. The error is
// ErrAllowanceSpent when the day's quota has no room for the next write, or a
// failure that ends the run.
func (run *run) push(m merged) error {
	held, err := run.held(m)
	if err != nil || held {
		return err
	}
	var writes []merge.Write
	switch m.sort {
	case merge.Manual:
		writes = merge.Plan(m.read, m.entries)
	case merge.Automatic:
		writes = merge.PlanAppends(m.read, m.entries)
	default:
		return fmt.Errorf("push playlist %s sorted %q, which is not a sort a push knows", m.id, m.sort)
	}
	base := unplaced(m.read)
	for _, w := range writes {
		if err := run.ctx.Err(); err != nil {
			return err
		}
		room, err := run.allows()
		if err != nil {
			return err
		}
		if !room {
			return ErrAllowanceSpent
		}
		next, goesOn, err := run.write(m, base, w)
		if err != nil || !goesOn {
			return err
		}
		base = next
	}
	return nil
}

// held is whether the playlist's push waits on a write YouTube refused this
// Pacific day, which it records as a failure of the playlist. A refusal on an
// earlier day is tried again, since some refusals pass.
func (run *run) held(m merged) (bool, error) {
	if !m.refusedWriteID.Valid {
		return false, nil
	}
	refused, err := run.store.Queries.GetYouTubeWrite(run.ctx, m.refusedWriteID.Int64)
	if err != nil {
		return false, fmt.Errorf("read the refused write %d to %s: %w", m.refusedWriteID.Int64, m.id, err)
	}
	if refused.QuotaDate != youtube.QuotaDate(run.now()) {
		return false, nil
	}
	run.report.Failures = append(run.report.Failures, Failure{
		Stage:    run.stage,
		Playlist: youtube.PlaylistID(m.id),
		Err:      fmt.Errorf("%w: YouTube refused the %s write %d: %s", ErrPushHeld, refused.Method, refused.WriteID, refused.Error.String),
	})
	return true, nil
}

// allows is whether the day's quota has room for one more write beside what the
// day has spent and a read at this run's cost for every run left before the
// quota resets.
func (run *run) allows() (bool, error) {
	now := run.now()
	date := youtube.QuotaDate(now)
	written, err := run.store.Queries.SumWriteUnits(run.ctx, date)
	if err != nil {
		return false, err
	}
	recorded, err := run.store.Queries.SumRunReadUnits(run.ctx, date)
	if err != nil {
		return false, err
	}
	spent := written.Units + written.Pending*youtube.WriteUnits + recorded
	if date == run.date {
		spent += run.readUnits
	}
	runsLeft := int64(youtube.QuotaReset(now).Sub(now) / run.interval)
	return spent+youtube.WriteUnits+runsLeft*run.readUnits <= DailyQuota, nil
}

// write records w as pending and as the playlist's unanswered write, sends it,
// and settles it with YouTube's answer. It returns the base the answer leaves,
// and whether the push goes on: it does not once the write did not do what the
// plan expects, or once the playlist's order changed or the API deleted the
// playlist before the write began. The error is YouTube's quota refusal, or a
// failure of the store, each of which ends the run.
func (run *run) write(m merged, base []merge.BaseItem, w merge.Write) ([]merge.BaseItem, bool, error) {
	method, record, err := writeRecord(m.id, w)
	if err != nil {
		return nil, false, err
	}
	record.SentAt = run.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(run.ctx), recordTimeout)
	var writeID int64
	begun := false
	err = run.store.InTx(ctx, func(tx *store.Tx) error {
		state, err := tx.GetPlaylistState(ctx, m.id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The API deleted the playlist after the merge.
			return nil
		case err != nil:
			return err
		case state.Revision != m.revision:
			// The API edited the playlist's order after the merge.
			return nil
		}
		if writeID, err = tx.BeginWrite(ctx, record); err != nil {
			return err
		}
		begun = true
		return tx.SetUnansweredWrite(ctx, generated.SetUnansweredWriteParams{PlaylistID: m.id, UnansweredWriteID: sql.NullInt64{Int64: writeID, Valid: true}})
	})
	cancel()
	if err != nil || !begun {
		return nil, false, err
	}

	requests, units := run.channel.Requests(), run.channel.Units()
	ctx, cancel = context.WithTimeout(context.WithoutCancel(run.ctx), youtubeTimeout)
	made, sendErr := run.send(ctx, m.id, w)
	cancel()
	outcome := store.WriteOutcome(sendErr)
	spent := run.channel.Units() - units
	run.report.Writes++
	run.report.WriteUnits += spent
	answer, err := answerOf(outcome, w.Kind)
	if err != nil {
		return nil, false, err
	}
	next := base
	if answer.done {
		if next, err = applied(base, w, made); err != nil {
			return nil, false, err
		}
	}

	ctx, cancel = context.WithTimeout(context.WithoutCancel(run.ctx), recordTimeout)
	defer cancel()
	err = run.store.InTx(ctx, func(tx *store.Tx) error {
		err := tx.SettleWrite(ctx, store.Settlement{
			WriteID:    writeID,
			PlaylistID: m.id,
			ItemID:     string(made),
			Outcome:    outcome,
			SettledAt:  run.now(),
			Requests:   run.channel.Requests() - requests,
			Units:      spent,
			Err:        sendErr,
		})
		if err != nil {
			return err
		}
		state, err := tx.GetPlaylistState(ctx, m.id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The API deleted the playlist while the write was sent, which took
			// the base and the entries the answer would change with it, and the
			// next write finds it gone before it begins.
			return nil
		case err != nil:
			return err
		}
		switch {
		case answer.done:
			if err := tx.ReplaceBase(ctx, m.id, storeBase(next)); err != nil {
				return err
			}
			if made != "" {
				if _, err := tx.SetEntryItem(ctx, generated.SetEntryItemParams{EntryID: w.EntryID, ItemID: sql.NullString{String: string(made), Valid: true}}); err != nil {
					return err
				}
			}
		case errors.Is(sendErr, youtube.ErrVideoNotFound), errors.Is(sendErr, youtube.ErrVideoRefused):
			if err := dropEntry(ctx, tx, m.id, state.Revision, w.EntryID); err != nil {
				return err
			}
		case errors.Is(sendErr, youtube.ErrManualSortRequired):
			if err := tx.SetPlaylistSort(ctx, generated.SetPlaylistSortParams{PlaylistID: m.id, Sort: store.SortAutomatic}); err != nil {
				return err
			}
		case outcome == store.WriteRefused:
			if err := tx.SetRefusedWrite(ctx, generated.SetRefusedWriteParams{PlaylistID: m.id, RefusedWriteID: sql.NullInt64{Int64: writeID, Valid: true}}); err != nil {
				return err
			}
		}
		if !answer.answered {
			return nil
		}
		return tx.SetUnansweredWrite(ctx, generated.SetUnansweredWriteParams{PlaylistID: m.id})
	})
	if err != nil {
		return nil, false, fmt.Errorf("settle the %s write %d to %s: %w", method, writeID, m.id, err)
	}
	if outcome == store.WriteQuotaSpent {
		return nil, false, sendErr
	}
	if answer.failed {
		run.report.Failures = append(run.report.Failures, Failure{Stage: run.stage, Playlist: youtube.PlaylistID(m.id), Err: sendErr})
	}
	return next, answer.done, nil
}

// answer is what a push write's outcome means to the push.
type answer struct {
	// done is whether the write did what the plan expects.
	done bool
	// answered is whether YouTube's answer says what it did.
	answered bool
	// failed is whether the run records the write as a failure of the playlist.
	// A write whose target YouTube reports gone is not one, since the next
	// run's read shows it gone.
	failed bool
}

// answerOf is what a write of kind that ended as outcome means to the push.
func answerOf(outcome string, kind merge.WriteKind) (answer, error) {
	switch outcome {
	case store.WriteApplied:
		return answer{done: true, answered: true}, nil
	case store.WriteAbsent:
		return answer{done: kind == merge.Delete, answered: true}, nil
	case store.WriteRefused:
		return answer{answered: true, failed: true}, nil
	case store.WriteQuotaSpent:
		return answer{answered: true}, nil
	case store.WriteUnanswered:
		return answer{failed: true}, nil
	case store.WritePending:
		return answer{}, fmt.Errorf("a push write ended %q, the outcome of a write not yet sent", outcome)
	default:
		return answer{}, fmt.Errorf("a push write ended %q, which is not an outcome a push knows", outcome)
	}
}

// writeRecord is the method w calls and the record of w, with no send time.
func writeRecord(playlist string, w merge.Write) (string, store.Write, error) {
	record := store.Write{PlaylistID: playlist}
	switch w.Kind {
	case merge.Delete:
		record.Method = youtube.MethodPlaylistItemsDelete
		record.ItemID = w.ItemID
	case merge.Insert:
		record.Method = youtube.MethodPlaylistItemsInsert
		record.VideoID, record.EntryID = w.VideoID, w.EntryID
		record.Position = sql.NullInt64{Int64: w.Position, Valid: true}
	case merge.Append:
		record.Method = youtube.MethodPlaylistItemsInsert
		record.VideoID, record.EntryID = w.VideoID, w.EntryID
	case merge.Move:
		record.Method = youtube.MethodPlaylistItemsUpdate
		record.ItemID = w.ItemID
		record.Position = sql.NullInt64{Int64: w.Position, Valid: true}
	default:
		return "", store.Write{}, fmt.Errorf("push a write of kind %q to %s, which is not a kind a push makes", w.Kind, playlist)
	}
	return record.Method, record, nil
}

// send makes w on the playlist, and returns the item an insert or an append
// made.
func (run *run) send(ctx context.Context, playlist string, w merge.Write) (youtube.ItemID, error) {
	id := youtube.PlaylistID(playlist)
	switch w.Kind {
	case merge.Delete:
		return "", run.channel.DeleteItem(ctx, youtube.ItemID(w.ItemID))
	case merge.Insert:
		return run.channel.InsertItem(ctx, id, youtube.VideoID(w.VideoID), w.Position)
	case merge.Append:
		return run.channel.AppendItem(ctx, id, youtube.VideoID(w.VideoID))
	case merge.Move:
		item := youtube.Item{ID: youtube.ItemID(w.ItemID), PlaylistID: id, VideoID: youtube.VideoID(w.VideoID)}
		return "", run.channel.MoveItem(ctx, item, w.Position)
	default:
		return "", fmt.Errorf("send a write of kind %q to %s, which is not a kind a push makes", w.Kind, playlist)
	}
}

// applied is base after w did what the plan expects, an insert or an append
// making the item made. An item w inserts or moves is placed, since YouTube
// holds it where the write landed.
func applied(base []merge.BaseItem, w merge.Write, made youtube.ItemID) ([]merge.BaseItem, error) {
	next := slices.Clone(base)
	placed := merge.BaseItem{Item: merge.Item{ID: string(made), VideoID: w.VideoID}, Placed: true}
	switch w.Kind {
	case merge.Delete:
		return slices.DeleteFunc(next, func(item merge.BaseItem) bool { return item.ID == w.ItemID }), nil
	case merge.Insert:
		return slices.Insert(next, int(w.Position), placed), nil
	case merge.Append:
		return append(next, placed), nil
	case merge.Move:
		index := slices.IndexFunc(next, func(item merge.BaseItem) bool { return item.ID == w.ItemID })
		if index < 0 {
			return nil, fmt.Errorf("apply a move of %s, which the base does not hold", w.ItemID)
		}
		moved := next[index]
		moved.Placed = true
		return slices.Insert(slices.Delete(next, index, index+1), int(w.Position), moved), nil
	default:
		return nil, fmt.Errorf("apply a write of kind %q, which is not a kind a push makes", w.Kind)
	}
}

// dropEntry removes from the server's order of the playlist, at revision, the
// entry whose video YouTube refuses to add.
func dropEntry(ctx context.Context, tx *store.Tx, playlist string, revision, entryID int64) error {
	entries, err := store.Entries(ctx, tx.Queries, playlist)
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(slices.Clone(entries), func(e store.Entry) bool { return e.ID == entryID })
	if len(kept) == len(entries) {
		return nil
	}
	_, err = tx.ReplaceOrder(ctx, playlist, revision, kept)
	return err
}
