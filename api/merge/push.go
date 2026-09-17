package merge

import "slices"

// Push is the writes that turn a YouTube playlist holding base into one holding
// local, in the order they are made: every removal, then every move, then every
// insert. Each position counts the playlist as it stands after the writes before
// it, as YouTube counts it.
type Push struct {
	// Remove holds the base position of each item to delete, highest first, so
	// each position still names the item it named in the base.
	Remove []int
	// Moves reorder the items both lists hold.
	Moves []Move
	// Inserts add the videos only local holds, each at its position in local,
	// lowest first.
	Inserts []Insert
}

// Move places the item at base position From at position To. YouTube takes the
// item out of the playlist and puts it back at To.
type Move struct {
	From int
	To   int
}

// Insert adds VideoID at Position.
type Insert struct {
	VideoID  string
	Position int
}

// Empty is whether the push makes no write.
func (p Push) Empty() bool {
	return len(p.Remove) == 0 && len(p.Moves) == 0 && len(p.Inserts) == 0
}

// PlanPush is the fewest writes that make a playlist holding base hold local.
//
// A video leaves by the position of its copy in the base, since an id cannot say
// which copy is meant. The copies both lists hold are reordered with the fewest
// moves, leaving the longest run already in local's order where it is. Each video
// only local holds is then inserted straight into its place, so it needs no move.
func PlanPush(base, local []string) Push {
	baseKeys, localKeys := Keyed(base), Keyed(local)
	inBase, inLocal := setOf(baseKeys), setOf(localKeys)
	basePosition := make(map[Key]int, len(baseKeys))
	for i, key := range baseKeys {
		basePosition[key] = i
	}

	var push Push
	for i := len(baseKeys) - 1; i >= 0; i-- {
		if !inLocal[baseKeys[i]] {
			push.Remove = append(push.Remove, i)
		}
	}

	kept := slices.DeleteFunc(slices.Clone(baseKeys), func(k Key) bool { return !inLocal[k] })
	desired := slices.DeleteFunc(slices.Clone(localKeys), func(k Key) bool { return !inBase[k] })
	for _, move := range reorder(kept, desired) {
		push.Moves = append(push.Moves, Move{From: basePosition[move.key], To: move.to})
	}

	for i, key := range localKeys {
		if !inBase[key] {
			push.Inserts = append(push.Inserts, Insert{VideoID: key.VideoID, Position: i})
		}
	}
	return push
}

// keyMove places key at position to.
type keyMove struct {
	key Key
	to  int
}

// reorder is the fewest moves turning current into desired, which hold the same
// keys. The keys in the longest subsequence of current already in desired's
// order stay put, and every other key moves. The moves run from the end of
// desired to its start, so the key each one is placed in front of is already
// where desired puts it.
func reorder(current, desired []Key) []keyMove {
	target := make(map[Key]int, len(desired))
	for i, key := range desired {
		target[key] = i
	}
	order := make([]int, len(current))
	for i, key := range current {
		order[i] = target[key]
	}
	stays := map[Key]bool{}
	for _, i := range longestIncreasing(order) {
		stays[current[i]] = true
	}

	playlist := slices.Clone(current)
	var moves []keyMove
	for i := len(desired) - 1; i >= 0; i-- {
		key := desired[i]
		if stays[key] {
			continue
		}
		playlist = slices.DeleteFunc(playlist, func(k Key) bool { return k == key })
		to := len(playlist)
		if i+1 < len(desired) {
			to = slices.Index(playlist, desired[i+1])
		}
		playlist = slices.Insert(playlist, to, key)
		moves = append(moves, keyMove{key: key, to: to})
	}
	return moves
}

// longestIncreasing is the indexes of a longest strictly increasing subsequence
// of values, in increasing order, found by patience sorting in O(n log n).
func longestIncreasing(values []int) []int {
	// tails[n] is the index of the smallest value ending an increasing run of
	// length n+1, and previous[i] the index before i in the run ending at i.
	var tails []int
	previous := make([]int, len(values))
	for i, value := range values {
		length, _ := slices.BinarySearchFunc(tails, value, func(tail, v int) int { return values[tail] - v })
		previous[i] = -1
		if length > 0 {
			previous[i] = tails[length-1]
		}
		if length == len(tails) {
			tails = append(tails, i)
		} else {
			tails[length] = i
		}
	}
	if len(tails) == 0 {
		return nil
	}
	run := make([]int, len(tails))
	for i, at := len(tails)-1, tails[len(tails)-1]; i >= 0; i, at = i-1, previous[at] {
		run[i] = at
	}
	return run
}
