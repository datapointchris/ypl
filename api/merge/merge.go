// Package merge decides what syncing one playlist does: how YouTube's read of it
// merges into the server's order, and which writes make YouTube hold the
// result. It reads no store and makes no request.
//
// A merge has three inputs. The base is what YouTube held after the server last
// read or wrote the playlist, the read is what YouTube holds now, and the
// entries are the server's order. Every one of them is keyed by YouTube's
// playlistItem id, which a copy of a video keeps wherever it moves. The base is
// what gives the other two meaning:
//
//   - an item of the base the read lacks was removed on YouTube
//   - an item of the base no entry holds was removed here
//   - an item of the read the base lacks was added on YouTube
//   - an entry holding no item was added here
//
// Order is settled once for the whole playlist, because a playlist ordered half
// by one side and half by the other is worse than either order.
package merge

import "slices"

// Item is one item of a playlist on YouTube: its playlistItem id and the video
// in it.
type Item struct {
	ID      string
	VideoID string
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

// BaseState is whether a playlist's base is known to be what YouTube held after
// the server's last read or write, as the store records it.
type BaseState int

const (
	// Confirmed is a base the server recorded every write's answer against.
	Confirmed BaseState = iota
	// Unconfirmed is a base a write was sent against whose answer was never
	// recorded, so YouTube may hold that write too.
	Unconfirmed
)

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

// Merge merges the read of a playlist into its entries against its base. Every
// item of the base the read lacks has to have been confirmed gone before it is
// passed, since a read that spans pages can miss an item that is there.
//
// An Unconfirmed base can miss the effect of the writes whose answers were
// lost. So each entry added here adopts an item of the read that holds its video
// and that the base and every other entry lack, as the insert that was sent
// would have made; and the server's order wins, since a move that landed would
// otherwise read as YouTube reordering the playlist. An item YouTube added in the
// same window with the same video as an entry added here is taken for that
// entry's insert, which is the one edit this loses.
//
// An entry holding an item neither the base nor the read has becomes an entry
// added here again, so its video is pushed rather than lost.
func Merge(base, read []Item, entries []Entry, state BaseState) Result {
	merging := slices.Clone(entries)
	if state == Unconfirmed {
		merging = adopt(base, read, merging)
	}
	inBase, inRead := itemSet(base), itemSet(read)
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
	for _, item := range read {
		k := key{item: item.ID}
		readKeys = append(readKeys, k)
		videoOf[item.ID] = item.VideoID
		switch {
		case inBase[item.ID] && !held[item.ID]:
		case !inBase[item.ID] && !held[item.ID]:
			result.Added++
			wanted[k] = true
		default:
			wanted[k] = true
		}
	}

	primary, secondary := serverKeys, readKeys
	if state == Confirmed && reordered(base, read) {
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
	result.Changed = !slices.Equal(result.Entries, entries)
	return result
}

// adopt gives each entry added here the first item of read that holds its
// video, that base lacks, and that no entry holds or has adopted.
func adopt(base, read []Item, entries []Entry) []Entry {
	inBase := itemSet(base)
	taken := map[string]bool{}
	for _, entry := range entries {
		if entry.ItemID != "" {
			taken[entry.ItemID] = true
		}
	}
	for i, entry := range entries {
		if entry.ItemID != "" {
			continue
		}
		for _, item := range read {
			if item.VideoID == entry.VideoID && !inBase[item.ID] && !taken[item.ID] {
				entries[i].ItemID = item.ID
				taken[item.ID] = true
				break
			}
		}
	}
	return entries
}

// reordered is whether read moved any item it shares with base. Only the items
// both hold can answer that: comparing whole lists would call every addition and
// removal a reorder, and hand YouTube the order of a playlist whenever a video
// was added to it there.
func reordered(base, read []Item) bool {
	inBase, inRead := itemSet(base), itemSet(read)
	shared := func(items []Item, in map[string]bool) []string {
		var ids []string
		for _, item := range items {
			if in[item.ID] {
				ids = append(ids, item.ID)
			}
		}
		return ids
	}
	return !slices.Equal(shared(base, inRead), shared(read, inBase))
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
