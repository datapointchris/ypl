// Package merge decides what syncing one playlist does: how YouTube's read of it
// merges into the server's order, and which writes make YouTube hold the
// result. It reads no store and makes no request.
//
// A merge has three inputs. The base is what YouTube held after the server last
// read the playlist, with each write the server made since, the read is what
// YouTube holds now, and the entries are the server's order. Every one of them
// is keyed by YouTube's playlistItem id, which a copy of a video keeps wherever
// it moves. The base is what gives the other two meaning:
//
//   - an item of the base the read lacks was removed on YouTube
//   - an item of the base no entry holds was removed here
//   - an item of the read the base lacks was added on YouTube
//   - an entry holding no item was added here
//
// Order is settled once for the whole playlist, because a playlist ordered half
// by one side and half by the other is worse than either order.
package merge

import (
	"fmt"
	"slices"
)

// Item is one item of a playlist on YouTube: its playlistItem id and the video
// in it.
type Item struct {
	ID      string
	VideoID string
}

// BaseItem is one item of a playlist's base, and whether its position is where
// a write the server made put it, which no read has shown. YouTube holds such an
// item where the write landed, and an edit on YouTube while the write was sent
// shifts where that is.
type BaseItem struct {
	Item
	Placed bool
}

// Entry is one entry of the server's order of a playlist: its id, the video in
// it, and the id of the item holding the video on YouTube, which is empty for an
// entry added here and not yet pushed. An entry whose ID is 0 is new.
type Entry struct {
	ID      int64
	VideoID string
	ItemID  string
}

// Side names whose order a merge kept.
type Side string

const (
	// Server is the server's order.
	Server Side = "server"
	// YouTube is YouTube's order.
	YouTube Side = "youtube"
)

// Sort is how YouTube orders a playlist.
type Sort string

const (
	// Manual is a playlist YouTube keeps in the order writes put it in.
	Manual Sort = "manual"
	// Automatic is a playlist YouTube orders itself, which refuses a write
	// naming a position.
	Automatic Sort = "automatic"
)

// Playlist is what a merge of one playlist reads.
type Playlist struct {
	// Base is what YouTube held after the server last read the playlist, with
	// each write the server made since.
	Base []BaseItem
	// Read is what YouTube holds now. Every item of Base the read lacks has to
	// have been confirmed gone before the merge, since a read that spans pages
	// can miss an item that is there.
	Read []Item
	// Entries is the server's order.
	Entries []Entry
	// Unanswered is the write sent against Base whose answer was never
	// recorded, so YouTube may hold it too, and nil when there is none.
	Unanswered *Write
	// Sort is how YouTube orders the playlist.
	Sort Sort
}

// Result is what a merge decided.
type Result struct {
	// Entries is the server's order after the merge.
	Entries []Entry
	// Order is the side whose order Entries keeps.
	Order Side
	// Added is how many items YouTube added that the merge gave an entry, and
	// Removed how many entries the merge dropped because YouTube removed their
	// item.
	Added, Removed int
	// Changed is whether Entries differs from the entries merged.
	Changed bool
}

// Merge merges the read of a playlist into its entries against its base.
//
// YouTube's order wins when YouTube orders the playlist itself, and when it
// moved an item it placed itself among those the base and the read share. The
// server's order wins otherwise.
//
// An insert whose answer was lost may have made an item. The first item of the
// read holding its video that the base lacks and no entry holds is taken for
// it: the entry the insert was sent for holds it, or, when an edit here removed
// that entry, the item is removed here. So an item YouTube added with the same
// video before the read is taken for the insert, which is one edit a merge
// loses. A move on YouTube of an item the server placed, before a read shows
// the placement, is the other.
//
// An entry holding an item neither the base nor the read has becomes an entry
// added here again, so its video is pushed rather than lost.
func Merge(p Playlist) (Result, error) {
	if p.Sort != Manual && p.Sort != Automatic {
		return Result{}, fmt.Errorf("merge a playlist sorted %q, which is not a sort a merge knows", p.Sort)
	}
	merging := slices.Clone(p.Entries)
	inBase, inRead := map[string]bool{}, itemSet(p.Read)
	for _, item := range p.Base {
		inBase[item.ID] = true
	}
	madeHere, err := landedInsert(p, merging)
	if err != nil {
		return Result{}, err
	}
	held := map[string]bool{}
	for i, entry := range merging {
		if entry.ItemID != "" && !inBase[entry.ItemID] && !inRead[entry.ItemID] {
			merging[i].ItemID = ""
		}
		if merging[i].ItemID != "" {
			held[merging[i].ItemID] = true
		}
	}

	// A key is an item's id, or a pending entry's place among the entries.
	type key struct {
		item    string
		pending int
	}
	entryKey := func(i int) key {
		if merging[i].ItemID == "" {
			return key{pending: i + 1}
		}
		return key{item: merging[i].ItemID}
	}
	wanted := map[key]bool{}
	var serverKeys, readKeys []key
	result := Result{Order: Server}
	for i, entry := range merging {
		k := entryKey(i)
		serverKeys = append(serverKeys, k)
		if entry.ItemID != "" && inBase[entry.ItemID] && !inRead[entry.ItemID] {
			result.Removed++
			continue
		}
		wanted[k] = true
	}
	videoOf := map[string]string{}
	for _, item := range p.Read {
		k := key{item: item.ID}
		readKeys = append(readKeys, k)
		videoOf[item.ID] = item.VideoID
		switch {
		case held[item.ID]:
			wanted[k] = true
		case inBase[item.ID], item.ID == madeHere:
		default:
			result.Added++
			wanted[k] = true
		}
	}

	primary, secondary := serverKeys, readKeys
	if p.Sort == Automatic || reordered(p) {
		result.Order = YouTube
		primary, secondary = readKeys, serverKeys
	}
	entryOf := map[string]Entry{}
	for _, entry := range merging {
		if entry.ItemID != "" {
			entryOf[entry.ItemID] = entry
		}
	}
	for _, k := range weave(primary, secondary, wanted) {
		switch {
		case k.pending > 0:
			result.Entries = append(result.Entries, merging[k.pending-1])
		case entryOf[k.item].ItemID != "":
			result.Entries = append(result.Entries, entryOf[k.item])
		default:
			result.Entries = append(result.Entries, Entry{VideoID: videoOf[k.item], ItemID: k.item})
		}
	}
	result.Changed = !slices.Equal(result.Entries, p.Entries)
	return result, nil
}

// landedInsert settles in entries the item an unanswered insert made, if the
// read shows one: the entry the insert was sent for takes it while that entry
// is still waiting for an item. It returns the item when no entry takes it,
// since an edit here removed the entry, and empty otherwise.
func landedInsert(p Playlist, entries []Entry) (string, error) {
	w := p.Unanswered
	if w == nil {
		return "", nil
	}
	switch w.Kind {
	case Insert, Append:
	case Delete, Move:
		return "", nil
	default:
		return "", fmt.Errorf("merge after an unanswered write of kind %q, which is not a kind a merge knows", w.Kind)
	}
	taken := map[string]bool{}
	for _, item := range p.Base {
		taken[item.ID] = true
	}
	for _, entry := range entries {
		if entry.ItemID != "" {
			taken[entry.ItemID] = true
		}
	}
	index := slices.IndexFunc(p.Read, func(item Item) bool { return item.VideoID == w.VideoID && !taken[item.ID] })
	if index < 0 {
		return "", nil
	}
	made := p.Read[index].ID
	for i, entry := range entries {
		if entry.ID == w.EntryID && entry.ItemID == "" {
			entries[i].ItemID = made
			return "", nil
		}
	}
	return made, nil
}

// reordered is whether YouTube moved an item it placed itself among those the
// base and the read share. Only those can answer it. Comparing whole lists would
// call every addition and removal a reorder. An item a write here placed sits
// where the write landed, which an edit on YouTube while it was sent shifts, and
// so does the item an unanswered move names.
func reordered(p Playlist) bool {
	unplaced := map[string]bool{}
	for _, item := range p.Base {
		unplaced[item.ID] = !item.Placed
	}
	if w := p.Unanswered; w != nil && w.Kind == Move {
		unplaced[w.ItemID] = false
	}
	inRead := itemSet(p.Read)
	var base, read []string
	for _, item := range p.Base {
		if unplaced[item.ID] && inRead[item.ID] {
			base = append(base, item.ID)
		}
	}
	for _, item := range p.Read {
		if unplaced[item.ID] {
			read = append(read, item.ID)
		}
	}
	return !slices.Equal(base, read)
}

// weave is the wanted keys of primary in its order, with each wanted key only
// secondary holds placed directly after the key that precedes it in secondary.
//
// The insertion point only moves forward. Where the two orders disagree about
// the keys they share, a key secondary holds after all of them lands at the end,
// where secondary had it, rather than beside an anchor primary placed earlier.
func weave[K comparable](primary, secondary []K, wanted map[K]bool) []K {
	result := slices.DeleteFunc(slices.Clone(primary), func(k K) bool { return !wanted[k] })
	cursor := -1
	for _, k := range secondary {
		if !wanted[k] {
			continue
		}
		if index := slices.Index(result, k); index >= 0 {
			cursor = max(cursor, index)
			continue
		}
		cursor++
		result = slices.Insert(result, cursor, k)
	}
	return result
}

func itemSet(items []Item) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item.ID] = true
	}
	return set
}
