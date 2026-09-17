package merge

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// slot is one item in a simulated YouTube playlist: its video, and its position
// in the base, or -1 for an item a push inserted.
type slot struct {
	videoID string
	base    int
}

// apply makes push's writes to a playlist holding base, as YouTube makes them,
// and fails t on a position YouTube refuses: a removal or move naming no item,
// a move to the item count or past it, or an insert past the item count.
func apply(t *testing.T, base []string, push Push) []string {
	t.Helper()
	var playlist []slot
	for i, id := range base {
		playlist = append(playlist, slot{videoID: id, base: i})
	}
	find := func(base int) int {
		return slices.IndexFunc(playlist, func(s slot) bool { return s.base == base })
	}
	for _, position := range push.Remove {
		at := find(position)
		if at != position {
			t.Fatalf("removal of base position %d finds it at %d", position, at)
		}
		playlist = slices.Delete(playlist, at, at+1)
	}
	for _, move := range push.Moves {
		at := find(move.From)
		if at < 0 || move.To < 0 || move.To >= len(playlist) {
			t.Fatalf("move %+v on a playlist of %d is refused", move, len(playlist))
		}
		item := playlist[at]
		playlist = slices.Insert(slices.Delete(playlist, at, at+1), move.To, item)
	}
	for _, insert := range push.Inserts {
		if insert.Position < 0 || insert.Position > len(playlist) {
			t.Fatalf("insert %+v on a playlist of %d is refused", insert, len(playlist))
		}
		playlist = slices.Insert(playlist, insert.Position, slot{videoID: insert.VideoID, base: -1})
	}
	var videos []string
	for _, s := range playlist {
		videos = append(videos, s.videoID)
	}
	return videos
}

func planned(base, local string) Push {
	return PlanPush(ids(base), ids(local))
}

func TestAPlaylistThatMatchesItsBaseNeedsNoPush(t *testing.T) {
	if push := planned("abc", "abc"); !push.Empty() {
		t.Fatalf("push %+v, want none", push)
	}
}

func TestAVideoAddedHereIsInsertedAtItsPlace(t *testing.T) {
	push := planned("abc", "abcd")
	if !slices.Equal(push.Inserts, []Insert{{VideoID: "d", Position: 3}}) || len(push.Remove) > 0 || len(push.Moves) > 0 {
		t.Fatalf("push %+v, want d inserted at 3 and nothing else", push)
	}
}

// A video id cannot say which of its copies is meant, and a position can.
func TestAVideoDeletedHereIsRemovedByItsPositionInTheBase(t *testing.T) {
	push := planned("aba", "ab")
	if !slices.Equal(push.Remove, []int{2}) || len(push.Inserts) > 0 || len(push.Moves) > 0 {
		t.Fatalf("push %+v, want base position 2 removed and nothing else", push)
	}
}

func TestRemovalsRunFromTheEndSoEachPositionStillNamesItsItem(t *testing.T) {
	push := planned("abcde", "ace")
	if !slices.Equal(push.Remove, []int{3, 1}) {
		t.Fatalf("removals %v, want 3 then 1", push.Remove)
	}
}

func TestAReorderAloneIsOnlyMoves(t *testing.T) {
	push := planned("abc", "cba")
	if len(push.Remove) > 0 || len(push.Inserts) > 0 || push.Empty() {
		t.Fatalf("push %+v, want moves and nothing else", push)
	}
}

// Placing each addition directly costs no move, where appending and moving it
// would cost one each.
func TestAdditionsAroundTheBaseNeedNoMoves(t *testing.T) {
	push := planned("ab", "xaby")
	want := []Insert{{VideoID: "x", Position: 0}, {VideoID: "y", Position: 3}}
	if !slices.Equal(push.Inserts, want) || len(push.Moves) > 0 {
		t.Fatalf("push %+v, want x at 0 and y at 3 with no moves", push)
	}
}

func TestAFirstPushOfANewPlaylistIsAllInserts(t *testing.T) {
	push := planned("", "abc")
	want := []Insert{{VideoID: "a", Position: 0}, {VideoID: "b", Position: 1}, {VideoID: "c", Position: 2}}
	if !slices.Equal(push.Inserts, want) || len(push.Remove) > 0 || len(push.Moves) > 0 {
		t.Fatalf("push %+v, want a, b and c inserted in order", push)
	}
}

func TestEveryPlanProducesTheOrderItPromised(t *testing.T) {
	cases := [][2]string{
		{"abc", "abc"},
		{"abc", "cba"},
		{"abcd", "badc"},
		{"a", "a"},
		{"", ""},
		{"abcde", "eabcd"},
		{"abcde", "bcdea"},
		{"abcdefghij", "jihgfedcba"},
		{"abcdefghij", "acbdfeghji"},
		{"aba", "baa"},
		{"abcdef", "xfaey"},
		{"abc", ""},
	}
	for _, c := range cases {
		if got := apply(t, ids(c[0]), planned(c[0], c[1])); !slices.Equal(got, ids(c[1])) {
			t.Errorf("pushing %q onto %q gives %q", c[1], c[0], got)
		}
	}
}

func TestAnOrderThatIsAlreadyRightCostsNothing(t *testing.T) {
	if push := planned("abcde", "abcde"); len(push.Moves) > 0 {
		t.Fatalf("moves %+v, want none", push.Moves)
	}
}

// Rewriting every slot would be ten writes, and each costs 50 units.
func TestMovingOneItemToTheFrontIsOneMove(t *testing.T) {
	if push := planned("abcdefghij", "jabcdefghi"); len(push.Moves) != 1 {
		t.Fatalf("moves %+v, want one", push.Moves)
	}
}

func TestTheMovesAreAsFewAsTheReorderAllows(t *testing.T) {
	cases := []struct {
		base, local string
		moves       int
	}{
		{"abcde", "abcde", 0},
		{"abcde", "bacde", 1},
		{"abcde", "abecd", 1},
		{"abcdef", "feabcd", 2},
		{"abcde", "edcba", 4},
	}
	for _, c := range cases {
		if push := planned(c.base, c.local); len(push.Moves) != c.moves {
			t.Errorf("%q to %q plans %d moves, want %d", c.base, c.local, len(push.Moves), c.moves)
		}
	}
}

func TestAHalfRotationOfThreeHundredItemsIsOneHundredAndFiftyMoves(t *testing.T) {
	base := make([]string, 300)
	for i := range base {
		base[i] = fmt.Sprintf("v%03d", i)
	}
	local := append(slices.Clone(base[150:]), base[:150]...)
	push := PlanPush(base, local)
	if len(push.Moves) != 150 {
		t.Fatalf("planned %d moves, want 150", len(push.Moves))
	}
	if got := apply(t, base, push); !slices.Equal(got, local) {
		t.Fatal("the plan did not produce the rotation")
	}
}

// Random base and local lists over a four-video alphabet, so copies of one
// video are common. Every plan has to produce local, and move only the items
// outside a longest run already in local's order.
func TestEveryRandomPlanProducesLocalWithTheFewestMoves(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	draw := func() []string {
		videos := make([]string, random.IntN(12))
		for i := range videos {
			videos[i] = string(rune('a' + random.IntN(4)))
		}
		return videos
	}
	for range 2000 {
		base, local := draw(), draw()
		push := PlanPush(base, local)
		if got := apply(t, base, push); !slices.Equal(got, local) {
			t.Fatalf("pushing %q onto %q gives %q", local, base, got)
		}
		if want := fewestMoves(base, local); len(push.Moves) != want {
			t.Fatalf("pushing %q onto %q plans %d moves, want %d", local, base, len(push.Moves), want)
		}
	}
}

// fewestMoves is the kept items less the longest run of them already in local's
// order, found by the quadratic longest-increasing-subsequence walk rather than
// the patience sort under test.
func fewestMoves(base, local []string) int {
	baseKeys, localKeys := Keyed(base), Keyed(local)
	target := map[Key]int{}
	for _, key := range slices.DeleteFunc(localKeys, func(k Key) bool { return !slices.Contains(baseKeys, k) }) {
		target[key] = len(target)
	}
	var order []int
	for _, key := range baseKeys {
		if position, kept := target[key]; kept {
			order = append(order, position)
		}
	}
	longest := 0
	runs := make([]int, len(order))
	for i := range order {
		runs[i] = 1
		for j := range i {
			if order[j] < order[i] {
				runs[i] = max(runs[i], runs[j]+1)
			}
		}
		longest = max(longest, runs[i])
	}
	return len(order) - longest
}
