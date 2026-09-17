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
// playlist's order changes after the merge. The error is ErrAllowanceSpent when
// the day's quota has no room for the next write, or a failure that ends the
// run.
func (run *run) push(m merged) error {
	writes := merge.Plan(m.base, m.entries)
	if m.sort == store.SortAutomatic {
		writes = merge.PlanAppends(m.base, m.entries)
	}
	base := slices.Clone(m.base)
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
		state, err := run.store.Queries.GetPlaylistState(run.ctx, m.id)
		if err != nil {
			return err
		}
		if state.Revision != m.revision {
			return nil
		}
		next, done, err := run.write(m, base, w)
		if err != nil || !done {
			return err
		}
		base = next
	}
	return nil
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

// write records w as pending, marking the playlist's base unconfirmed, sends it,
// and settles it with YouTube's answer. It returns the base the answer leaves,
// and whether the write did what the plan expects. A write that did not is
// recorded as a failure of the playlist, unless the next run's merge accounts
// for what it did. The error is YouTube's quota refusal, or a failure of the
// store, each of which ends the run.
func (run *run) write(m merged, base []merge.Item, w merge.Write) ([]merge.Item, bool, error) {
	method, record := writeRecord(m.id, w)
	record.SentAt = run.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(run.ctx), recordTimeout)
	var writeID int64
	err := run.store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if writeID, err = tx.BeginWrite(ctx, record); err != nil {
			return err
		}
		return tx.SetBaseState(ctx, generated.SetBaseStateParams{PlaylistID: m.id, BaseState: store.BaseUnconfirmed})
	})
	cancel()
	if err != nil {
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

	next := base
	done := outcome == store.WriteApplied || (outcome == store.WriteAbsent && w.Kind == merge.Delete)
	if done {
		next = applied(base, w, made)
	}
	confirmed := outcome != store.WriteUnanswered
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
		switch {
		case done:
			if err := tx.ReplaceBase(ctx, m.id, storeBase(next)); err != nil {
				return err
			}
			if made != "" {
				if _, err := tx.SetEntryItem(ctx, generated.SetEntryItemParams{EntryID: w.EntryID, ItemID: sql.NullString{String: string(made), Valid: true}}); err != nil {
					return err
				}
			}
		case errors.Is(sendErr, youtube.ErrVideoNotFound), errors.Is(sendErr, youtube.ErrVideoRefused):
			if err := dropEntry(ctx, tx, m.id, w.EntryID); err != nil {
				return err
			}
		case errors.Is(sendErr, youtube.ErrManualSortRequired):
			if err := tx.SetPlaylistSort(ctx, generated.SetPlaylistSortParams{PlaylistID: m.id, Sort: store.SortAutomatic}); err != nil {
				return err
			}
		}
		if !confirmed {
			return nil
		}
		return tx.SetBaseState(ctx, generated.SetBaseStateParams{PlaylistID: m.id, BaseState: store.BaseCurrent})
	})
	if err != nil {
		return nil, false, fmt.Errorf("settle the %s write %d to %s: %w", method, writeID, m.id, err)
	}

	switch {
	case done:
	case outcome == store.WriteQuotaSpent:
		return nil, false, sendErr
	case outcome == store.WriteRefused && !errors.Is(sendErr, youtube.ErrManualSortRequired),
		outcome == store.WriteUnanswered:
		run.report.Failures = append(run.report.Failures, Failure{Playlist: youtube.PlaylistID(m.id), Err: sendErr})
	}
	return next, done, nil
}

// writeRecord is the method w calls and the record of w, with no send time.
func writeRecord(playlist string, w merge.Write) (string, store.Write) {
	record := store.Write{PlaylistID: playlist, ItemID: w.ItemID, VideoID: w.VideoID}
	switch w.Kind {
	case merge.Delete:
		record.Method = youtube.MethodPlaylistItemsDelete
		record.VideoID = ""
	case merge.Insert:
		record.Method = youtube.MethodPlaylistItemsInsert
		record.Position = sql.NullInt64{Int64: w.Position, Valid: true}
	case merge.Append:
		record.Method = youtube.MethodPlaylistItemsInsert
	case merge.Move:
		record.Method = youtube.MethodPlaylistItemsUpdate
		record.VideoID = ""
		record.Position = sql.NullInt64{Int64: w.Position, Valid: true}
	}
	return record.Method, record
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
	default:
		item := youtube.Item{ID: youtube.ItemID(w.ItemID), PlaylistID: id, VideoID: youtube.VideoID(w.VideoID)}
		return "", run.channel.MoveItem(ctx, item, w.Position)
	}
}

// applied is base after w did what the plan expects, an insert or an append
// making the item made.
func applied(base []merge.Item, w merge.Write, made youtube.ItemID) []merge.Item {
	next := slices.Clone(base)
	switch w.Kind {
	case merge.Delete:
		return slices.DeleteFunc(next, func(item merge.Item) bool { return item.ID == w.ItemID })
	case merge.Insert:
		return slices.Insert(next, int(w.Position), merge.Item{ID: string(made), VideoID: w.VideoID})
	case merge.Append:
		return append(next, merge.Item{ID: string(made), VideoID: w.VideoID})
	default:
		index := slices.IndexFunc(next, func(item merge.Item) bool { return item.ID == w.ItemID })
		moved := next[index]
		next = slices.Delete(next, index, index+1)
		return slices.Insert(next, int(w.Position), moved)
	}
}

// dropEntry removes from the server's order of the playlist the entry whose
// video YouTube refuses to add, counting the change in its revision.
func dropEntry(ctx context.Context, tx *store.Tx, playlist string, entryID int64) error {
	entries, err := store.Entries(ctx, tx.Queries, playlist)
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(entries, func(e store.Entry) bool { return e.ID == entryID })
	if len(kept) == len(entries) {
		return nil
	}
	state, err := tx.GetPlaylistState(ctx, playlist)
	if err != nil {
		return err
	}
	if err := tx.ReplaceEntries(ctx, playlist, kept); err != nil {
		return err
	}
	return bumpRevision(ctx, tx, playlist, state.Revision)
}
