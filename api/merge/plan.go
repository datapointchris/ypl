package merge

import (
	"fmt"
	"slices"
)

// WriteKind names what a push write does to a playlist.
type WriteKind string

const (
	// Delete removes an item.
	Delete WriteKind = "delete"
	// Insert adds an entry's video at a position.
	Insert WriteKind = "insert"
	// Append adds an entry's video at the end.
	Append WriteKind = "append"
	// Move moves an item to a position.
	Move WriteKind = "move"
)

// Write is one write a push makes. A delete names an item. An insert and an
// append name the entry whose video they add. A move names an item and its
// video. An insert and a move name the position the item lands at, counting
// from 0 in the playlist as the writes before it leave it.
type Write struct {
	Kind     WriteKind
	ItemID   string
	EntryID  int64
	VideoID  string
	Position int64
}

func (w Write) String() string {
	switch w.Kind {
	case Delete:
		return fmt.Sprintf("delete %s", w.ItemID)
	case Insert:
		return fmt.Sprintf("insert entry %d (%s) at %d", w.EntryID, w.VideoID, w.Position)
	case Append:
		return fmt.Sprintf("append entry %d (%s)", w.EntryID, w.VideoID)
	default:
		return fmt.Sprintf("move %s to %d", w.ItemID, w.Position)
	}
}

// Plan is the writes, in the order to make them, that turn base, what YouTube
// holds, into entries, a playlist sorted manually. Every item an entry holds
// has to be in base, as a merge leaves it.
//
// It deletes each item of base no entry holds, keeps in place the longest run of
// held items already in entries' order, then walks entries inserting each entry
// added here and moving each other held item to just after the entry before it.
// That is the fewest writes for single-item moves.
func Plan(base []Item, entries []Entry) []Write {
	writes, playlist := deletes(base, entries)
	position := map[string]int{}
	for i, id := range playlist {
		position[id] = i
	}
	var kept []string
	for _, entry := range entries {
		if _, ok := position[entry.ItemID]; ok && entry.ItemID != "" {
			kept = append(kept, entry.ItemID)
		}
	}
	stays := map[string]bool{}
	for _, id := range longestIncreasing(kept, position) {
		stays[id] = true
	}

	previous := ""
	after := func() int64 {
		if previous == "" {
			return 0
		}
		return int64(slices.Index(playlist, previous) + 1)
	}
	for index, entry := range entries {
		switch {
		case entry.ItemID == "":
			key := pendingKey(index)
			at := after()
			playlist = slices.Insert(playlist, int(at), key)
			writes = append(writes, Write{Kind: Insert, EntryID: entry.ID, VideoID: entry.VideoID, Position: at})
			previous = key
		case stays[entry.ItemID]:
			previous = entry.ItemID
		default:
			playlist = slices.DeleteFunc(playlist, func(id string) bool { return id == entry.ItemID })
			at := after()
			playlist = slices.Insert(playlist, int(at), entry.ItemID)
			writes = append(writes, Write{Kind: Move, ItemID: entry.ItemID, VideoID: entry.VideoID, Position: at})
			previous = entry.ItemID
		}
	}
	return writes
}

// PlanAppends is the writes that bring base toward entries in a playlist YouTube
// orders itself, which refuses a write naming a position: it deletes each item
// of base no entry holds and appends each entry added here, and moves nothing.
func PlanAppends(base []Item, entries []Entry) []Write {
	writes, _ := deletes(base, entries)
	for _, entry := range entries {
		if entry.ItemID == "" {
			writes = append(writes, Write{Kind: Append, EntryID: entry.ID, VideoID: entry.VideoID})
		}
	}
	return writes
}

// deletes is a delete of each item of base no entry holds, in base's order, and
// the ids of the items left.
func deletes(base []Item, entries []Entry) ([]Write, []string) {
	held := map[string]bool{}
	for _, entry := range entries {
		if entry.ItemID != "" {
			held[entry.ItemID] = true
		}
	}
	var writes []Write
	var left []string
	for _, item := range base {
		if held[item.ID] {
			left = append(left, item.ID)
			continue
		}
		writes = append(writes, Write{Kind: Delete, ItemID: item.ID})
	}
	return writes, left
}

// longestIncreasing is the longest subsequence of ids whose positions increase,
// the first found among the longest.
func longestIncreasing(ids []string, position map[string]int) []string {
	// tails[k] is the index into ids of the smallest last position of an
	// increasing run of length k+1, and previous[i] the index before i in the
	// run ending at i.
	var tails []int
	previous := make([]int, len(ids))
	for i, id := range ids {
		k, _ := slices.BinarySearchFunc(tails, position[id], func(tail, target int) int {
			return position[ids[tail]] - target
		})
		previous[i] = -1
		if k > 0 {
			previous[i] = tails[k-1]
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	run := make([]string, len(tails))
	if len(tails) == 0 {
		return run
	}
	for i, k := tails[len(tails)-1], len(tails)-1; k >= 0; k-- {
		run[k] = ids[i]
		i = previous[i]
	}
	return run
}

// pendingKey is the key the entry at index takes in the playlist a plan walks
// once it is inserted, which no item id can equal.
func pendingKey(index int) string {
	return fmt.Sprintf("\x00entry %d", index)
}
