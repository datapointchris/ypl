package reconcile

import (
	"errors"
	"slices"
	"time"

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// push makes the writes that turn the playlist's base into the server's order,
// within the allowance of write units left for the day, and records the base
// after each write. A failure about this playlist is recorded and stops its
// push; the error is for one that ends the run.
func (run *run) push(id youtube.PlaylistID, allowance *int64) error {
	if err := run.ctx.Err(); err != nil {
		return err
	}
	q := run.store.Queries
	playlist, err := q.GetPlaylist(run.ctx, string(id))
	if err != nil {
		return err
	}
	rows, err := q.ListBaseItems(run.ctx, string(id))
	if err != nil {
		return err
	}
	local, err := q.ListPlaylistVideoIDs(run.ctx, string(id))
	if err != nil {
		return err
	}
	refused, err := q.ListPushRefusedVideoIDs(run.ctx, string(id))
	if err != nil {
		return err
	}
	base := make([]store.BaseItem, len(rows))
	for i, row := range rows {
		base[i] = store.BaseItem{ItemID: row.ItemID, VideoID: row.VideoID}
	}
	plan := merge.PlanPush(baseVideoIDs(rows), withoutRefused(baseVideoIDs(rows), local, refused))
	if plan.Empty() {
		return nil
	}
	p := &pusher{run: run, id: id, original: slices.Clone(base), base: base, sortedManually: playlist.IsSortedManually, allowance: allowance}
	return p.apply(plan)
}

// withoutRefused is local less every copy of a refused video the base does not
// already hold, so a push asks YouTube for none of them again.
func withoutRefused(base, local, refused []string) []string {
	inBase := map[merge.Key]bool{}
	for _, key := range merge.Keyed(base) {
		inBase[key] = true
	}
	var kept []string
	for _, key := range merge.Keyed(local) {
		if !slices.Contains(refused, key.VideoID) || inBase[key] {
			kept = append(kept, key.VideoID)
		}
	}
	return kept
}

// pusher makes one playlist's writes.
type pusher struct {
	run *run
	id  youtube.PlaylistID
	// original is the base the plan was made from, which a move names its item
	// by position in; base is what YouTube holds after the writes so far.
	original       []store.BaseItem
	base           []store.BaseItem
	sortedManually bool
	allowance      *int64
}

// apply makes the plan's removals, moves and inserts in order. It stops without
// a failure when the day's allowance cannot cover another write.
func (p *pusher) apply(plan merge.Push) error {
	for _, position := range plan.Remove {
		if !p.affordable() {
			return nil
		}
		item := p.base[position]
		err := p.write(func() error { return p.run.channel.DeleteItem(p.run.ctx, youtube.ItemID(item.ItemID)) })
		if err != nil && !errors.Is(err, youtube.ErrItemNotFound) {
			return p.fail(err)
		}
		p.base = slices.Delete(p.base, position, position+1)
		if err := p.saveBase(err == nil); err != nil {
			return err
		}
	}

	for _, move := range plan.Moves {
		if !p.sortedManually {
			break
		}
		if !p.affordable() {
			return nil
		}
		item := p.original[move.From]
		err := p.write(func() error {
			return p.run.channel.MoveItem(p.run.ctx, youtube.Item{
				ID: youtube.ItemID(item.ItemID), PlaylistID: p.id, VideoID: youtube.VideoID(item.VideoID),
			}, int64(move.To))
		})
		switch {
		case errors.Is(err, youtube.ErrManualSortRequired):
			if err := p.markNotSortedManually(); err != nil {
				return err
			}
		case err != nil:
			return p.fail(err)
		default:
			at := slices.Index(p.base, item)
			p.base = slices.Insert(slices.Delete(p.base, at, at+1), move.To, item)
			if err := p.saveBase(true); err != nil {
				return err
			}
		}
	}

	// A refused video is left out, so each insert after one lands a place earlier
	// than the plan put it.
	refused := 0
	for _, insert := range plan.Inserts {
		for {
			if !p.affordable() {
				return nil
			}
			position := insert.Position - refused
			if !p.sortedManually {
				position = len(p.base)
			}
			var added youtube.ItemID
			err := p.write(func() error {
				var err error
				if p.sortedManually {
					added, err = p.run.channel.InsertItem(p.run.ctx, p.id, youtube.VideoID(insert.VideoID), int64(position))
				} else {
					added, err = p.run.channel.AppendItem(p.run.ctx, p.id, youtube.VideoID(insert.VideoID))
				}
				return err
			})
			switch {
			case errors.Is(err, youtube.ErrManualSortRequired) && p.sortedManually:
				if err := p.markNotSortedManually(); err != nil {
					return err
				}
				continue
			case errors.Is(err, youtube.ErrVideoNotFound), errors.Is(err, youtube.ErrVideoRefused):
				refused++
				if err := p.refuse(insert.VideoID, err); err != nil {
					return err
				}
			case err != nil:
				return p.fail(err)
			default:
				p.base = slices.Insert(p.base, position, store.BaseItem{ItemID: string(added), VideoID: insert.VideoID})
				if err := p.saveBase(true); err != nil {
					return err
				}
			}
			break
		}
	}
	return nil
}

// affordable is whether the day's allowance covers one more write.
func (p *pusher) affordable() bool {
	return *p.allowance >= youtube.WriteUnits
}

// write makes one write through do, and charges what its requests cost to the
// run's write units and the day's allowance.
func (p *pusher) write(do func() error) error {
	before := p.run.channel.Units()
	err := do()
	spent := p.run.channel.Units() - before
	p.run.report.WriteUnits += spent
	*p.allowance -= spent
	return err
}

// saveBase records what YouTube now holds, counting a write when one was made.
func (p *pusher) saveBase(written bool) error {
	if written {
		p.run.report.Writes++
	}
	return p.run.store.InTx(p.run.ctx, func(tx *store.Tx) error {
		return tx.ReplaceBaseItems(p.run.ctx, string(p.id), p.base)
	})
}

func (p *pusher) markNotSortedManually() error {
	p.sortedManually = false
	return p.run.store.Queries.MarkPlaylistNotSortedManually(p.run.ctx, string(p.id))
}

func (p *pusher) refuse(video string, err error) error {
	return p.run.store.Queries.UpsertPushRefusal(p.run.ctx, generated.UpsertPushRefusalParams{
		PlaylistID: string(p.id),
		VideoID:    video,
		RefusedTs:  p.run.now().UTC().Format(time.RFC3339),
		Reason:     err.Error(),
	})
}

// fail records err against the playlist and stops its push, unless err ends the
// run, which it returns.
func (p *pusher) fail(err error) error {
	if endsRun(p.run.ctx, err) {
		return err
	}
	p.run.report.Failures = append(p.run.report.Failures, Failure{Playlist: p.id, Err: err})
	return nil
}
