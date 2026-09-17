// Package merge decides what reconciling one playlist does, from lists of video
// ids. It reads no store and makes no request.
//
// A reconcile has three inputs: the base, what YouTube held at the last
// reconcile; remote, what it holds now; and local, what the server holds. The
// base is what gives the other two meaning. Local [A B C] against remote [A C]
// has two histories: B was deleted on YouTube, or B was added here and never
// pushed. A video in the base and missing from a side was deleted by that side,
// and a video on a side and missing from the base was added by it.
//
// Membership merges per copy of a video. Order is settled once for the whole
// playlist, because a playlist ordered half by one side and half by the other
// is worse than either order.
package merge

import "slices"

// Side names where a merged order came from.
type Side string

const (
	// Local is the server's order.
	Local Side = "local"
	// Remote is YouTube's order.
	Remote Side = "remote"
)

// Key is one copy of a video in a playlist: its id, and how many copies of it
// precede this one. A playlist can hold a video twice, and the copy number
// keeps the second copy distinct from the first.
type Key struct {
	VideoID string
	Copy    int
}

// Keyed numbers each video by which copy of itself it is, in order.
func Keyed(videoIDs []string) []Key {
	seen := map[string]int{}
	keys := make([]Key, len(videoIDs))
	for i, id := range videoIDs {
		keys[i] = Key{VideoID: id, Copy: seen[id]}
		seen[id]++
	}
	return keys
}

// Result is what a merge decided.
type Result struct {
	// Order is the playlist as the server holds it after the merge.
	Order []string
	// PulledIn holds the videos only YouTube added, in YouTube's order.
	PulledIn []string
	// PulledOut holds the videos YouTube deleted that the server still held, in
	// the base's order.
	PulledOut []string
	// PendingAdd holds the videos only the server added, in the server's order.
	PendingAdd []string
	// PendingRemove holds the videos the server deleted that YouTube still
	// holds, in the base's order.
	PendingRemove []string
	// OrderSource is the side whose order Order keeps.
	OrderSource Side
}

// ChangedHere is whether the merge changed what the server holds.
func (r Result) ChangedHere() bool {
	return len(r.PulledIn) > 0 || len(r.PulledOut) > 0
}

// ToPush is whether YouTube is missing a membership change the server made.
func (r Result) ToPush() bool {
	return len(r.PendingAdd) > 0 || len(r.PendingRemove) > 0
}

// Merge reconciles one playlist's base, remote and local lists of video ids.
//
// An empty base means the playlist has never been reconciled, and the result is
// the union of both sides. With nothing recorded, no absence can be read as a
// deletion.
//
// Remote's order wins when YouTube reordered the videos the base and remote
// share. Otherwise the server's order stands. A video only one side holds is
// placed directly after whatever precedes it in that side's order.
func Merge(base, remote, local []string) Result {
	baseKeys, remoteKeys, localKeys := Keyed(base), Keyed(remote), Keyed(local)
	inBase, inRemote, inLocal := setOf(baseKeys), setOf(remoteKeys), setOf(localKeys)

	// A key in the base that one side lacks was deleted by that side, and stays
	// deleted whatever the other side holds.
	wanted := map[Key]bool{}
	for _, keys := range [][]Key{remoteKeys, localKeys} {
		for _, key := range keys {
			if !inBase[key] || (inRemote[key] && inLocal[key]) {
				wanted[key] = true
			}
		}
	}

	source := Local
	primary, secondary := localKeys, remoteKeys
	if reordered(baseKeys, remoteKeys) {
		source = Remote
		primary, secondary = remoteKeys, localKeys
	}

	return Result{
		Order: videoIDs(weave(primary, secondary, wanted)),
		// A key both sides added needs nothing done to it, and pushing it would
		// put the video in the playlist twice.
		PulledIn:      videoIDsWhere(remoteKeys, func(k Key) bool { return wanted[k] && !inBase[k] && !inLocal[k] }),
		PulledOut:     videoIDsWhere(baseKeys, func(k Key) bool { return !inRemote[k] && inLocal[k] }),
		PendingAdd:    videoIDsWhere(localKeys, func(k Key) bool { return wanted[k] && !inBase[k] && !inRemote[k] }),
		PendingRemove: videoIDsWhere(baseKeys, func(k Key) bool { return !inLocal[k] && inRemote[k] }),
		OrderSource:   source,
	}
}

// reordered is whether other moved any key it shares with base. Only the keys
// both hold can answer that: comparing whole lists would call every addition
// and deletion a reorder, and hand YouTube the order of a playlist whenever a
// video was added to it there.
func reordered(base, other []Key) bool {
	inBase, inOther := setOf(base), setOf(other)
	shared := func(keys []Key, in map[Key]bool) []Key {
		return slices.DeleteFunc(slices.Clone(keys), func(k Key) bool { return !in[k] })
	}
	return !slices.Equal(shared(base, inOther), shared(other, inBase))
}

// weave is the keys of wanted in primary's order, with each key only secondary
// holds placed directly after the key that precedes it in secondary.
//
// The insertion point only moves forward. Where the two orders disagree about
// the keys they share, a key secondary holds after all of them lands at the
// end, where secondary had it, rather than beside an anchor primary placed
// earlier.
func weave(primary, secondary []Key, wanted map[Key]bool) []Key {
	result := slices.DeleteFunc(slices.Clone(primary), func(k Key) bool { return !wanted[k] })
	cursor := -1
	for _, key := range secondary {
		if !wanted[key] {
			continue
		}
		if index := slices.Index(result, key); index >= 0 {
			cursor = max(cursor, index)
			continue
		}
		cursor++
		result = slices.Insert(result, cursor, key)
	}
	return result
}

func setOf(keys []Key) map[Key]bool {
	set := make(map[Key]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return set
}

func videoIDs(keys []Key) []string {
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.VideoID
	}
	return ids
}

func videoIDsWhere(keys []Key, keep func(Key) bool) []string {
	ids := []string{}
	for _, key := range keys {
		if keep(key) {
			ids = append(ids, key.VideoID)
		}
	}
	return ids
}
