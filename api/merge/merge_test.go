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

// baseItems is a base written as items are, with a * after each item a write
// here placed, "i1:a i2:b*".
func baseItems(s string) []BaseItem {
	var list []BaseItem
	for _, field := range strings.Fields(s) {
		unmarked, placed := strings.CutSuffix(field, "*")
		id, video, _ := strings.Cut(unmarked, ":")
		list = append(list, BaseItem{Item: Item{ID: id, VideoID: video}, Placed: placed})
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
		unanswered          *Write
		sort                Sort
		want                string
		order               Side
		added, removed      int
	}{
		{
			name: "nothing changed",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "1=i1:a 2=i2:b", sort: Manual,
			want: "1=i1:a 2=i2:b", order: Server,
		},
		{
			name: "an item added on YouTube lands after its predecessor there",
			base: "i1:a i2:b", read: "i1:a i3:c i2:b", entries: "1=i1:a 2=i2:b", sort: Manual,
			want: "1=i1:a 0=i3:c 2=i2:b", order: Server, added: 1,
		},
		{
			name: "an item removed on YouTube leaves the server's order",
			base: "i1:a i2:b", read: "i2:b", entries: "1=i1:a 2=i2:b", sort: Manual,
			want: "2=i2:b", order: Server, removed: 1,
		},
		{
			name: "an item removed here stays removed",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "2=i2:b", sort: Manual,
			want: "2=i2:b", order: Server,
		},
		{
			name: "an entry added here stays",
			base: "i1:a", read: "i1:a", entries: "3=:c 1=i1:a", sort: Manual,
			want: "3=:c 1=i1:a", order: Server,
		},
		{
			name: "YouTube's reorder wins over the server's",
			base: "i1:a i2:b i3:c", read: "i3:c i1:a i2:b", entries: "2=i2:b 1=i1:a 3=i3:c", sort: Manual,
			want: "3=i3:c 1=i1:a 2=i2:b", order: YouTube,
		},
		{
			name: "the server's reorder stands beside an addition on YouTube",
			base: "i1:a i2:b", read: "i1:a i2:b i3:c", entries: "2=i2:b 1=i1:a", sort: Manual,
			want: "2=i2:b 1=i1:a 0=i3:c", order: Server, added: 1,
		},
		{
			name: "a removal on YouTube of one copy of a repeated video is not a reorder",
			base: "i1:a i2:b i3:a", read: "i2:b i3:a", entries: "3=i3:a 2=i2:b", sort: Manual,
			want: "3=i3:a 2=i2:b", order: Server, removed: 0,
		},
		{
			name: "an addition on YouTube of a second copy is its own entry",
			base: "i1:a", read: "i1:a i2:a", entries: "1=i1:a", sort: Manual,
			want: "1=i1:a 0=i2:a", order: Server, added: 1,
		},
		{
			name: "an entry holding an item neither side has is pushed again",
			base: "i1:a", read: "i1:a", entries: "1=i1:a 2=i9:b", sort: Manual,
			want: "1=i1:a 2=:b", order: Server,
		},
		{
			name: "an insert whose answer was lost and that landed is taken by its entry",
			base: "i1:a", read: "i1:a i2:b", entries: "1=i1:a 2=:b", sort: Manual,
			unanswered: &Write{Kind: Insert, EntryID: 2, VideoID: "b", Position: 1},
			want:       "1=i1:a 2=i2:b", order: Server,
		},
		{
			name: "an insert whose answer was lost and that did not land stays to be pushed",
			base: "i1:a", read: "i1:a", entries: "1=i1:a 2=:b", sort: Manual,
			unanswered: &Write{Kind: Insert, EntryID: 2, VideoID: "b", Position: 1},
			want:       "1=i1:a 2=:b", order: Server,
		},
		{
			name: "an insert whose answer was lost, for an entry removed here, is removed here",
			base: "i1:a i2:b", read: "i1:a i2:b i3:d", entries: "1=i1:a 2=i2:b", sort: Manual,
			unanswered: &Write{Kind: Append, EntryID: 3, VideoID: "d"},
			want:       "1=i1:a 2=i2:b", order: Server,
		},
		{
			name: "an item YouTube added for a video another entry here waits for is an addition",
			base: "i1:a", read: "i1:a i2:b", entries: "1=i1:a 2=:b 3=:c", sort: Manual,
			unanswered: &Write{Kind: Insert, EntryID: 3, VideoID: "c", Position: 1},
			want:       "1=i1:a 0=i2:b 2=:b 3=:c", order: Server, added: 1,
		},
		{
			name: "an insert whose answer was lost is taken by no entry but the one it was sent for",
			base: "i1:a", read: "i1:a i2:d", entries: "1=i1:a 4=:d", sort: Manual,
			unanswered: &Write{Kind: Insert, EntryID: 3, VideoID: "d", Position: 1},
			want:       "1=i1:a 4=:d", order: Server,
		},
		{
			name: "a move whose answer was lost and that landed is not a reorder",
			base: "i1:a i2:b i3:c", read: "i2:b i1:a i3:c", entries: "3=i3:c 2=i2:b 1=i1:a", sort: Manual,
			unanswered: &Write{Kind: Move, ItemID: "i2", VideoID: "b", Position: 0},
			want:       "3=i3:c 2=i2:b 1=i1:a", order: Server,
		},
		{
			name: "YouTube's reorder wins beside a move whose answer was lost",
			base: "i1:a i2:b i3:c", read: "i3:c i1:a i2:b", entries: "2=i2:b 1=i1:a 3=i3:c", sort: Manual,
			unanswered: &Write{Kind: Move, ItemID: "i2", VideoID: "b", Position: 0},
			want:       "3=i3:c 1=i1:a 2=i2:b", order: YouTube,
		},
		{
			name: "a placed item YouTube holds elsewhere is not a reorder",
			base: "i1:a i4:x* i2:b", read: "i5:y i4:x i1:a i2:b", entries: "1=i1:a 4=i4:x 2=i2:b", sort: Manual,
			want: "0=i5:y 1=i1:a 4=i4:x 2=i2:b", order: Server, added: 1,
		},
		{
			name: "YouTube's reorder of the items it placed wins beside a placed item",
			base: "i1:a i2:b* i3:c", read: "i3:c i2:b i1:a", entries: "1=i1:a 2=i2:b 3=i3:c", sort: Manual,
			want: "3=i3:c 2=i2:b 1=i1:a", order: YouTube,
		},
		{
			name: "a removal on YouTube still removes beside an unanswered insert",
			base: "i1:a i2:b", read: "i2:b", entries: "1=i1:a 2=i2:b 3=:z", sort: Manual,
			unanswered: &Write{Kind: Insert, EntryID: 3, VideoID: "z", Position: 2},
			want:       "2=i2:b 3=:z", order: Server, removed: 1,
		},
		{
			name: "a delete whose answer was lost and that did not land is deleted again",
			base: "i1:a i2:b", read: "i1:a i2:b", entries: "2=i2:b", sort: Manual,
			unanswered: &Write{Kind: Delete, ItemID: "i1"},
			want:       "2=i2:b", order: Server,
		},
		{
			name: "a playlist YouTube orders itself takes YouTube's order",
			base: "i1:a i2:b i3:c", read: "i1:a i2:b i3:c", entries: "3=i3:c 1=i1:a 2=i2:b", sort: Automatic,
			want: "1=i1:a 2=i2:b 3=i3:c", order: YouTube,
		},
		{
			name: "a playlist YouTube orders itself keeps an entry added here after its predecessor here",
			base: "i1:a i2:b", read: "i2:b i1:a", entries: "1=i1:a 5=:x 2=i2:b", sort: Automatic,
			want: "2=i2:b 1=i1:a 5=:x", order: YouTube,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Merge(Playlist{Base: baseItems(c.base), Read: items(c.read), Entries: entries(c.entries), Unanswered: c.unanswered, Sort: c.sort})
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
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

func TestAMergeOfAnUnknownSortOrWriteIsRefused(t *testing.T) {
	for name, p := range map[string]Playlist{
		"no sort":            {Base: baseItems("i1:a"), Read: items("i1:a")},
		"an unknown sort":    {Base: baseItems("i1:a"), Read: items("i1:a"), Sort: "shuffled"},
		"an unknown write":   {Base: baseItems("i1:a"), Read: items("i1:a"), Sort: Manual, Unanswered: &Write{Kind: "copy"}},
		"a write of no kind": {Base: baseItems("i1:a"), Read: items("i1:a"), Sort: Manual, Unanswered: &Write{}},
	} {
		if result, err := Merge(p); err == nil {
			t.Errorf("%s: Merge = %+v, want it refused", name, result)
		}
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name, read, entries string
		want                []string
	}{
		{name: "nothing to do", read: "i1:a i2:b", entries: "1=i1:a 2=i2:b"},
		{name: "a removal of one copy is one delete", read: "i1:a i2:b i3:a", entries: "2=i2:b 3=i3:a", want: []string{"delete i1"}},
		{name: "one item moved to the end is one move", read: "i1:a i2:b i3:c i4:d", entries: "2=i2:b 3=i3:c 4=i4:d 1=i1:a", want: []string{"move i1 to 3"}},
		{name: "one item moved to the start is one move", read: "i1:a i2:b i3:c", entries: "3=i3:c 1=i1:a 2=i2:b", want: []string{"move i3 to 0"}},
		{
			name: "an insert lands after the entry before it",
			read: "i1:a i2:b", entries: "1=i1:a 5=:x 2=i2:b 6=:y",
			want: []string{"insert entry 5 (x) at 1", "insert entry 6 (y) at 3"},
		},
		{
			name: "deletes come first and positions follow them",
			read: "i1:a i2:b i3:c", entries: "3=i3:c 5=:x",
			want: []string{"delete i1", "delete i2", "insert entry 5 (x) at 1"},
		},
		{name: "an insert into an empty playlist is at 0", read: "", entries: "5=:x", want: []string{"insert entry 5 (x) at 0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, w := range Plan(items(c.read), entries(c.entries)) {
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
		playlist = applyOne(t, playlist, w, fmt.Sprintf("entry %d", w.EntryID))
	}
	return playlist
}

// applyOne makes w on playlist as YouTube does, an insert or an append making
// the item made.
func applyOne(t *testing.T, playlist []string, w Write, made string) []string {
	t.Helper()
	switch w.Kind {
	case Delete:
		index := slices.Index(playlist, w.ItemID)
		if index < 0 {
			t.Fatalf("%s: no such item in %v", w, playlist)
		}
		return slices.Delete(playlist, index, index+1)
	case Insert:
		if w.Position < 0 || int(w.Position) > len(playlist) {
			t.Fatalf("%s: YouTube refuses a position past %d", w, len(playlist))
		}
		return slices.Insert(playlist, int(w.Position), made)
	case Append:
		return append(playlist, made)
	case Move:
		index := slices.Index(playlist, w.ItemID)
		if index < 0 || w.Position < 0 || int(w.Position) >= len(playlist) {
			t.Fatalf("%s: YouTube refuses a move in %v", w, playlist)
		}
		return slices.Insert(slices.Delete(playlist, index, index+1), int(w.Position), w.ItemID)
	default:
		t.Fatalf("%s: YouTube has no such write", w)
		return nil
	}
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
		var read []Item
		for i := range n {
			read = append(read, Item{ID: fmt.Sprintf("i%d", i), VideoID: fmt.Sprintf("v%d", random.IntN(4))})
		}
		var order []Entry
		for _, item := range read {
			if random.IntN(4) > 0 {
				order = append(order, Entry{ID: int64(len(order) + 1), VideoID: item.VideoID, ItemID: item.ID})
			}
		}
		random.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for range random.IntN(3) {
			at := random.IntN(len(order) + 1)
			order = slices.Insert(order, at, Entry{ID: int64(100 + trial*10 + at), VideoID: "new"})
		}

		readIDs := make([]string, len(read))
		for i, item := range read {
			readIDs[i] = item.ID
		}
		writes := Plan(read, order)
		if got, want := apply(t, readIDs, writes), named(order); !slices.Equal(got, want) {
			t.Fatalf("trial %d: read %v, order %s: writes %v leave %v, want %v", trial, readIDs, show(order), writes, got, want)
		}

		position := map[string]int{}
		for i, id := range readIDs {
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
			t.Fatalf("trial %d: %d moves for %v over %v, want %d", trial, moves, held, readIDs, want)
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

// Over random edits on both sides of a synced playlist, a push write of each
// kind whose answer was lost and random items placed by writes here, a merge
// keeps every addition and removal each side made, holds no item twice, keeps
// the order of the side it names, and a plan from YouTube's read leaves the
// merged order. An insert whose answer was lost is taken by its entry when the
// entry stays, and removed when an edit here removed the entry.
func TestAMergeKeepsEverySidesEdits(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))
	for trial := range 4000 {
		var base []BaseItem
		for i := range random.IntN(10) {
			base = append(base, BaseItem{Item: Item{ID: fmt.Sprintf("i%d", i), VideoID: fmt.Sprintf("v%d", random.IntN(4))}, Placed: random.IntN(4) == 0})
		}
		read := make([]Item, len(base))
		var server []Entry
		for i, item := range base {
			read[i] = item.Item
			server = append(server, Entry{ID: int64(i + 1), VideoID: item.VideoID, ItemID: item.ID})
		}
		sort := []Sort{Manual, Automatic}[random.IntN(2)]

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

		// The unanswered write is an insert of u for entry 50, which may have
		// landed and whose entry may since have been removed here, or a delete
		// or a move of an item of the base.
		var unanswered *Write
		landed, kept := random.IntN(2) == 0, random.IntN(2) == 0
		switch random.IntN(4) {
		case 0:
			unanswered = &Write{Kind: []WriteKind{Insert, Append}[random.IntN(2)], EntryID: 50, VideoID: "u"}
			if landed {
				read = slices.Insert(read, random.IntN(len(read)+1), Item{ID: "u1", VideoID: "u"})
			}
			if kept {
				server = slices.Insert(server, random.IntN(len(server)+1), Entry{ID: 50, VideoID: "u"})
			}
		case 1:
			if len(base) > 0 {
				item := base[random.IntN(len(base))]
				unanswered = &Write{Kind: []WriteKind{Delete, Move}[random.IntN(2)], ItemID: item.ID, VideoID: item.VideoID}
			}
		}

		result, err := Merge(Playlist{Base: base, Read: read, Entries: server, Unanswered: unanswered, Sort: sort})
		if err != nil {
			t.Fatalf("trial %d: Merge: %v", trial, err)
		}
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
		if result.Added != len(addedThere) {
			t.Fatalf("trial %d: Added = %d, want YouTube's %d additions", trial, result.Added, len(addedThere))
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
		if unanswered != nil && unanswered.EntryID == 50 {
			taken := slices.Contains(result.Entries, Entry{ID: 50, VideoID: "u", ItemID: "u1"})
			switch {
			case landed && kept && !taken:
				t.Fatalf("trial %d: the insert that landed is not taken by entry 50 in %s", trial, show(result.Entries))
			case landed && !kept && seen["u1"]:
				t.Fatalf("trial %d: the insert whose entry was removed here is back in %s", trial, show(result.Entries))
			case !landed && kept && !pending[50]:
				t.Fatalf("trial %d: the insert that did not land is not pending in %s", trial, show(result.Entries))
			}
		}

		if sort == Automatic && result.Order != YouTube {
			t.Fatalf("trial %d: a playlist YouTube orders itself kept the %s order", trial, result.Order)
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

// Over random edits here pushed while YouTube adds items between the writes, so
// that every write after an addition lands at a position the plan did not mean,
// the next merge keeps the server's order and every addition YouTube made, and
// a plan from that read leaves the merged order.
func TestAPushShiftedByAdditionsOnYouTubeKeepsTheServersOrder(t *testing.T) {
	random := rand.New(rand.NewPCG(5, 6))
	for trial := range 2000 {
		var read []Item
		var server []Entry
		for i := range random.IntN(10) {
			item := Item{ID: fmt.Sprintf("i%d", i), VideoID: fmt.Sprintf("v%d", random.IntN(4))}
			read = append(read, item)
			if random.IntN(4) > 0 {
				server = append(server, Entry{ID: int64(i + 1), VideoID: item.VideoID, ItemID: item.ID})
			}
		}
		random.Shuffle(len(server), func(i, j int) { server[i], server[j] = server[j], server[i] })
		for k := range random.IntN(3) {
			server = slices.Insert(server, random.IntN(len(server)+1), Entry{ID: int64(100 + k), VideoID: "new"})
		}

		// actual is what YouTube holds as the writes land, and predicted the base
		// the push stores, each write placing what it inserts or moves.
		actual := make([]string, len(read))
		predicted := make([]BaseItem, len(read))
		for i, item := range read {
			actual[i] = item.ID
			predicted[i] = BaseItem{Item: item}
		}
		video := map[string]string{}
		for _, item := range read {
			video[item.ID] = item.VideoID
		}
		pushed := slices.Clone(server)
		added := 0
		for _, w := range Plan(read, server) {
			for range random.IntN(2) {
				id := fmt.Sprintf("y%d", added)
				video[id] = "w"
				actual = slices.Insert(actual, random.IntN(len(actual)+1), id)
				added++
			}
			made := fmt.Sprintf("m%d", w.EntryID)
			actual = applyOne(t, actual, w, made)
			ids := make([]string, len(predicted))
			for i, item := range predicted {
				ids[i] = item.ID
			}
			placed := map[string]bool{}
			for _, item := range predicted {
				placed[item.ID] = item.Placed
			}
			switch w.Kind {
			case Insert, Append:
				video[made] = w.VideoID
				placed[made] = true
				index := slices.IndexFunc(pushed, func(e Entry) bool { return e.ID == w.EntryID })
				pushed[index].ItemID = made
			case Move:
				placed[w.ItemID] = true
			}
			predicted = nil
			for _, id := range applyOne(t, ids, w, made) {
				predicted = append(predicted, BaseItem{Item: Item{ID: id, VideoID: video[id]}, Placed: placed[id]})
			}
		}
		var shown []Item
		for _, id := range actual {
			shown = append(shown, Item{ID: id, VideoID: video[id]})
		}

		result, err := Merge(Playlist{Base: predicted, Read: shown, Entries: pushed, Sort: Manual})
		if err != nil {
			t.Fatalf("trial %d: Merge: %v", trial, err)
		}
		merged := named(result.Entries)
		ours := slices.DeleteFunc(slices.Clone(merged), func(id string) bool { return strings.HasPrefix(id, "y") })
		if result.Order != Server || result.Added != added || !slices.Equal(ours, named(pushed)) {
			t.Fatalf("trial %d: pushed %v onto %v, merged %v with order %s and %d added, want the server's order and %d added",
				trial, named(pushed), actual, merged, result.Order, result.Added, added)
		}
		if got := apply(t, actual, Plan(shown, result.Entries)); !slices.Equal(got, merged) {
			t.Fatalf("trial %d: the plan from the read leaves %v, want %v", trial, got, merged)
		}
	}
}
