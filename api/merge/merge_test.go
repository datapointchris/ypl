package merge

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// items is a playlist written as item id and video pairs, "i1:a i2:b".
func items(s string) []Item {
	var list []Item
	for _, field := range strings.Fields(s) {
		id, video, _ := strings.Cut(field, ":")
		list = append(list, Item{ID: id, VideoID: video})
	}
	return list
}

// entries is a server order written as entries, "1=i1:a 2=:b", where an empty
// item id is an entry added here.
func entries(s string) []Entry {
	var list []Entry
	for _, field := range strings.Fields(s) {
		id, rest, _ := strings.Cut(field, "=")
		item, video, _ := strings.Cut(rest, ":")
		var n int64
		_, _ = fmt.Sscan(id, &n)
		list = append(list, Entry{ID: n, VideoID: video, ItemID: item})
	}
	return list
}

func show(list []Entry) string {
	var fields []string
	for _, e := range list {
		fields = append(fields, fmt.Sprintf("%d=%s:%s", e.ID, e.ItemID, e.VideoID))
	}
	return strings.Join(fields, " ")
}

func TestMerge(t *testing.T) {
	cases := []struct {
		name                string
		base, read, entries string
		state               BaseState
		want                string
		order               Side
		added, removed      int
	}{
		{
			name: "nothing changed",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "1=i1:a 2=i2:b",
			want: "1=i1:a 2=i2:b", order: Server,
		},
		{
			name: "an item added on YouTube lands after its predecessor there",
			base: "i1:a i2:b", read: "i1:a i3:c i2:b", entries: "1=i1:a 2=i2:b",
			want: "1=i1:a 0=i3:c 2=i2:b", order: Server, added: 1,
		},
		{
			name: "an item removed on YouTube leaves the server's order",
			base: "i1:a i2:b", read: "i2:b", entries: "1=i1:a 2=i2:b",
			want: "2=i2:b", order: Server, removed: 1,
		},
		{
			name: "an item removed here stays removed",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "2=i2:b",
			want: "2=i2:b", order: Server,
		},
		{
			name: "an entry added here stays",
			base: "i1:a", read: "i1:a", entries: "3=:c 1=i1:a",
			want: "3=:c 1=i1:a", order: Server,
		},
		{
			name: "YouTube's reorder wins over the server's",
			base: "i1:a i2:b i3:c", read: "i3:c i1:a i2:b", entries: "2=i2:b 1=i1:a 3=i3:c",
			want: "3=i3:c 1=i1:a 2=i2:b", order: YouTube,
		},
		{
			name: "the server's reorder stands beside an addition on YouTube",
			base: "i1:a i2:b", read: "i1:a i2:b i3:c", entries: "2=i2:b 1=i1:a",
			want: "2=i2:b 1=i1:a 0=i3:c", order: Server, added: 1,
		},
		{
			name: "a removal on YouTube of one copy of a repeated video is not a reorder",
			base: "i1:a i2:b i3:a", read: "i2:b i3:a", entries: "3=i3:a 2=i2:b",
			want: "3=i3:a 2=i2:b", order: Server, removed: 0,
		},
		{
			name: "an addition on YouTube of a second copy is its own entry",
			base: "i1:a", read: "i1:a i2:a", entries: "1=i1:a",
			want: "1=i1:a 0=i2:a", order: Server, added: 1,
		},
		{
			name: "an entry holding an item neither side has is pushed again",
			base: "i1:a", read: "i1:a", entries: "1=i1:a 2=i9:b",
			want: "1=i1:a 2=:b", order: Server,
		},
		{
			name: "an unconfirmed insert that landed is adopted",
			base: "i1:a", read: "i1:a i2:b", entries: "1=i1:a 2=:b",
			state: Unconfirmed,
			want:  "1=i1:a 2=i2:b", order: Server,
		},
		{
			name: "an unconfirmed insert that did not land stays to be pushed",
			base: "i1:a", read: "i1:a", entries: "1=i1:a 2=:b",
			state: Unconfirmed,
			want:  "1=i1:a 2=:b", order: Server,
		},
		{
			name: "unconfirmed moves that landed part way do not read as a reorder",
			base: "i1:a i2:b i3:c", read: "i2:b i1:a i3:c", entries: "3=i3:c 2=i2:b 1=i1:a",
			state: Unconfirmed,
			want:  "3=i3:c 2=i2:b 1=i1:a", order: Server,
		},
		{
			name: "an item added on YouTube while unconfirmed with no entry for its video is an addition",
			base: "i1:a", read: "i1:a i2:c", entries: "1=i1:a",
			state: Unconfirmed,
			want:  "1=i1:a 0=i2:c", order: Server, added: 1,
		},
		{
			name: "a removal on YouTube while unconfirmed still removes",
			base: "i1:a i2:b", read: "i2:b", entries: "1=i1:a 2=i2:b",
			state: Unconfirmed,
			want:  "2=i2:b", order: Server, removed: 1,
		},
		{
			name: "an unconfirmed delete that did not land is deleted again",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "2=i2:b",
			state: Unconfirmed,
			want:  "2=i2:b", order: Server,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Merge(items(c.base), items(c.read), entries(c.entries), c.state)
			if show(got.Entries) != c.want || got.Order != c.order || got.Added != c.added || got.Removed != c.removed {
				t.Fatalf("Merge = %s, order %s, added %d, removed %d; want %s, order %s, added %d, removed %d",
					show(got.Entries), got.Order, got.Added, got.Removed, c.want, c.order, c.added, c.removed)
			}
			if got.Changed != (show(got.Entries) != show(entries(c.entries))) {
				t.Fatalf("Changed = %v for %s against %s", got.Changed, show(got.Entries), c.entries)
			}
		})
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name, base, entries string
		want                []string
	}{
		{name: "nothing to do", base: "i1:a i2:b", entries: "1=i1:a 2=i2:b"},
		{name: "a removal of one copy is one delete", base: "i1:a i2:b i3:a", entries: "2=i2:b 3=i3:a", want: []string{"delete i1"}},
		{name: "one item moved to the end is one move", base: "i1:a i2:b i3:c i4:d", entries: "2=i2:b 3=i3:c 4=i4:d 1=i1:a", want: []string{"move i1 to 3"}},
		{name: "one item moved to the start is one move", base: "i1:a i2:b i3:c", entries: "3=i3:c 1=i1:a 2=i2:b", want: []string{"move i3 to 0"}},
		{
			name: "an insert lands after the entry before it",
			base: "i1:a i2:b", entries: "1=i1:a 5=:x 2=i2:b 6=:y",
			want: []string{"insert entry 5 (x) at 1", "insert entry 6 (y) at 3"},
		},
		{
			name: "deletes come first and positions follow them",
			base: "i1:a i2:b i3:c", entries: "3=i3:c 5=:x",
			want: []string{"delete i1", "delete i2", "insert entry 5 (x) at 1"},
		},
		{name: "an insert into an empty playlist is at 0", base: "", entries: "5=:x", want: []string{"insert entry 5 (x) at 0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, w := range Plan(items(c.base), entries(c.entries)) {
				got = append(got, w.String())
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("Plan = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPlanAppendsDeletesAndAppendsAndMovesNothing(t *testing.T) {
	var got []string
	for _, w := range PlanAppends(items("i1:a i2:b i3:c"), entries("3=i3:c 5=:x 2=i2:b 6=:y")) {
		got = append(got, w.String())
	}
	if want := []string{"delete i1", "append entry 5 (x)", "append entry 6 (y)"}; !slices.Equal(got, want) {
		t.Fatalf("PlanAppends = %q, want %q", got, want)
	}
}

// apply makes writes on playlist as YouTube does, refusing a position YouTube
// refuses, and returns the playlist with each inserted item named by its entry.
func apply(t *testing.T, playlist []string, writes []Write) []string {
	t.Helper()
	playlist = slices.Clone(playlist)
	for _, w := range writes {
		switch w.Kind {
		case Delete:
			index := slices.Index(playlist, w.ItemID)
			if index < 0 {
				t.Fatalf("%s: no such item in %v", w, playlist)
			}
			playlist = slices.Delete(playlist, index, index+1)
		case Insert:
			if w.Position < 0 || int(w.Position) > len(playlist) {
				t.Fatalf("%s: YouTube refuses a position past %d", w, len(playlist))
			}
			playlist = slices.Insert(playlist, int(w.Position), fmt.Sprintf("entry %d", w.EntryID))
		case Append:
			playlist = append(playlist, fmt.Sprintf("entry %d", w.EntryID))
		case Move:
			index := slices.Index(playlist, w.ItemID)
			if index < 0 || w.Position < 0 || int(w.Position) >= len(playlist) {
				t.Fatalf("%s: YouTube refuses a move in %v", w, playlist)
			}
			playlist = slices.Delete(playlist, index, index+1)
			playlist = slices.Insert(playlist, int(w.Position), w.ItemID)
		}
	}
	return playlist
}

func named(list []Entry) []string {
	var keys []string
	for _, e := range list {
		if e.ItemID == "" {
			keys = append(keys, fmt.Sprintf("entry %d", e.ID))
		} else {
			keys = append(keys, e.ItemID)
		}
	}
	return keys
}

// Over random playlists, a plan made on YouTube leaves exactly the server's
// order, with a move for every held item outside the longest run already in
// order and no other move.
func TestAPlanMadeOnYouTubeLeavesTheServersOrder(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for trial := range 2000 {
		n := random.IntN(12)
		var base []Item
		for i := range n {
			base = append(base, Item{ID: fmt.Sprintf("i%d", i), VideoID: fmt.Sprintf("v%d", random.IntN(4))})
		}
		var order []Entry
		for _, item := range base {
			if random.IntN(4) > 0 {
				order = append(order, Entry{ID: int64(len(order) + 1), VideoID: item.VideoID, ItemID: item.ID})
			}
		}
		random.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for range random.IntN(3) {
			at := random.IntN(len(order) + 1)
			order = slices.Insert(order, at, Entry{ID: int64(100 + trial*10 + at), VideoID: "new"})
		}

		baseIDs := make([]string, len(base))
		for i, item := range base {
			baseIDs[i] = item.ID
		}
		writes := Plan(base, order)
		if got, want := apply(t, baseIDs, writes), named(order); !slices.Equal(got, want) {
			t.Fatalf("trial %d: base %v, order %s: writes %v leave %v, want %v", trial, baseIDs, show(order), writes, got, want)
		}

		position := map[string]int{}
		for i, id := range baseIDs {
			position[id] = i
		}
		var held []string
		for _, e := range order {
			if e.ItemID != "" {
				held = append(held, e.ItemID)
			}
		}
		moves := 0
		for _, w := range writes {
			if w.Kind == Move {
				moves++
			}
		}
		if want := len(held) - len(longestIncreasing(held, position)); moves != want || !increasing(longestIncreasing(held, position), position) {
			t.Fatalf("trial %d: %d moves for %v over %v, want %d", trial, moves, held, baseIDs, want)
		}
	}
}

func increasing(ids []string, position map[string]int) bool {
	for i := 1; i < len(ids); i++ {
		if position[ids[i]] <= position[ids[i-1]] {
			return false
		}
	}
	return true
}

// Over random edits on both sides of a synced playlist, a merge keeps every
// addition and removal each side made, holds no item twice, keeps the order of
// the side it names, and a plan from YouTube's read leaves the merged order.
func TestAMergeKeepsEverySidesEdits(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))
	for trial := range 2000 {
		var base []Item
		for i := range random.IntN(10) {
			base = append(base, Item{ID: fmt.Sprintf("i%d", i), VideoID: fmt.Sprintf("v%d", random.IntN(4))})
		}
		read := slices.Clone(base)
		var server []Entry
		for i, item := range base {
			server = append(server, Entry{ID: int64(i + 1), VideoID: item.VideoID, ItemID: item.ID})
		}

		removedThere := map[string]bool{}
		for i := len(read) - 1; i >= 0; i-- {
			if random.IntN(5) == 0 {
				removedThere[read[i].ID] = true
				read = slices.Delete(read, i, i+1)
			}
		}
		if random.IntN(3) == 0 {
			random.Shuffle(len(read), func(i, j int) { read[i], read[j] = read[j], read[i] })
		}
		addedThere := map[string]bool{}
		for k := range random.IntN(3) {
			id := fmt.Sprintf("y%d", k)
			addedThere[id] = true
			read = slices.Insert(read, random.IntN(len(read)+1), Item{ID: id, VideoID: fmt.Sprintf("v%d", random.IntN(4))})
		}
		removedHere := map[string]bool{}
		for i := len(server) - 1; i >= 0; i-- {
			if random.IntN(5) == 0 {
				removedHere[server[i].ItemID] = true
				server = slices.Delete(server, i, i+1)
			}
		}
		if random.IntN(3) == 0 {
			random.Shuffle(len(server), func(i, j int) { server[i], server[j] = server[j], server[i] })
		}
		addedHere := map[int64]bool{}
		for k := range random.IntN(3) {
			id := int64(100 + k)
			addedHere[id] = true
			server = slices.Insert(server, random.IntN(len(server)+1), Entry{ID: id, VideoID: "new"})
		}

		result := Merge(base, read, server, Confirmed)
		seen := map[string]bool{}
		pending := map[int64]bool{}
		for _, e := range result.Entries {
			if e.ItemID == "" {
				pending[e.ID] = true
				continue
			}
			if seen[e.ItemID] {
				t.Fatalf("trial %d: item %s held twice in %s", trial, e.ItemID, show(result.Entries))
			}
			seen[e.ItemID] = true
		}
		for id := range addedThere {
			if !seen[id] {
				t.Fatalf("trial %d: YouTube's addition %s is missing from %s", trial, id, show(result.Entries))
			}
		}
		for id := range addedHere {
			if !pending[id] {
				t.Fatalf("trial %d: the server's addition %d is missing from %s", trial, id, show(result.Entries))
			}
		}
		for id := range removedThere {
			if seen[id] {
				t.Fatalf("trial %d: YouTube's removal %s is back in %s", trial, id, show(result.Entries))
			}
		}
		for id := range removedHere {
			if seen[id] {
				t.Fatalf("trial %d: the server's removal %s is back in %s", trial, id, show(result.Entries))
			}
		}

		side := named(server)
		if result.Order == YouTube {
			side = nil
			for _, item := range read {
				side = append(side, item.ID)
			}
		}
		merged := named(result.Entries)
		if kept := slices.DeleteFunc(slices.Clone(merged), func(k string) bool { return !slices.Contains(side, k) }); !slices.Equal(kept, slices.DeleteFunc(slices.Clone(side), func(k string) bool { return !slices.Contains(merged, k) })) {
			t.Fatalf("trial %d: merged %v does not keep the %s order %v", trial, merged, result.Order, side)
		}

		readIDs := make([]string, len(read))
		for i, item := range read {
			readIDs[i] = item.ID
		}
		if got := apply(t, readIDs, Plan(read, result.Entries)); !slices.Equal(got, merged) {
			t.Fatalf("trial %d: the plan from the read leaves %v, want %v", trial, got, merged)
		}
	}
}
