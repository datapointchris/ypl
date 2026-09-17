package merge

import (
	"slices"
	"strings"
	"testing"
)

// ids is one video id per character of s.
func ids(s string) []string {
	return strings.Split(s, "")[:len(s)]
}

func merged(base, remote, local string) Result {
	return Merge(ids(base), ids(remote), ids(local))
}

func assertIDs(t *testing.T, name string, got []string, want string) {
	t.Helper()
	if !slices.Equal(got, ids(want)) {
		t.Errorf("%s = %q, want %q", name, got, ids(want))
	}
}

func sameMembers(got []string, want string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(ids(want))))
}

func TestNothingChangedAnywhere(t *testing.T) {
	result := merged("abc", "abc", "abc")
	assertIDs(t, "order", result.Order, "abc")
	if result.ChangedHere() || result.ToPush() {
		t.Errorf("result %+v, want nothing changed here and nothing to push", result)
	}
}

func TestAVideoGoneFromRemoteButInTheBaseWasDeletedThere(t *testing.T) {
	result := merged("abc", "ac", "abc")
	assertIDs(t, "order", result.Order, "ac")
	assertIDs(t, "pulled out", result.PulledOut, "b")
	if result.ToPush() {
		t.Errorf("result %+v, want nothing to push", result)
	}
}

// The same two lists as the test above, and the opposite correct action.
func TestAVideoMissingFromTheBaseWasAddedHereAndStillHasToGoUp(t *testing.T) {
	result := merged("ac", "ac", "abc")
	assertIDs(t, "order", result.Order, "abc")
	assertIDs(t, "pending add", result.PendingAdd, "b")
	if result.ChangedHere() {
		t.Errorf("result %+v, want nothing changed here", result)
	}
}

func TestAVideoDeletedHereStaysDeletedAndIsPending(t *testing.T) {
	result := merged("abc", "abc", "ac")
	assertIDs(t, "order", result.Order, "ac")
	assertIDs(t, "pending remove", result.PendingRemove, "b")
}

func TestAVideoAddedOnYouTubeArrivesHere(t *testing.T) {
	result := merged("ab", "abg", "ab")
	assertIDs(t, "order", result.Order, "abg")
	assertIDs(t, "pulled in", result.PulledIn, "g")
}

// Appending everything would move it, which reads as the server reordering the
// playlist.
func TestAVideoAddedAtTheFrontOnYouTubeArrivesAtTheFront(t *testing.T) {
	assertIDs(t, "order", merged("ab", "gab", "ab").Order, "gab")
}

func TestAVideoAddedInTheMiddleOnYouTubeLandsWhereItWasPut(t *testing.T) {
	assertIDs(t, "order", merged("abc", "agbc", "abc").Order, "agbc")
}

func TestBothSidesDeletingTheSameVideoIsNotAConflict(t *testing.T) {
	result := merged("abc", "ac", "ac")
	assertIDs(t, "order", result.Order, "ac")
	if result.ToPush() || result.ChangedHere() {
		t.Errorf("result %+v, want nothing to push and nothing changed here", result)
	}
}

// Pushing it again would put the same video in the playlist twice.
func TestBothSidesAddingTheSameVideoNeedsNothingDoing(t *testing.T) {
	result := merged("ab", "abg", "abg")
	assertIDs(t, "order", result.Order, "abg")
	assertIDs(t, "pulled in", result.PulledIn, "")
	if result.ToPush() {
		t.Errorf("result %+v, want nothing to push", result)
	}
}

// With no base recorded, no absence can be read as a deletion.
func TestAFirstReconcileUnionsRatherThanDeleting(t *testing.T) {
	result := merged("", "ag", "ab")
	if !sameMembers(result.Order, "abg") {
		t.Errorf("order = %q, want a, b and g", result.Order)
	}
	assertIDs(t, "pulled out", result.PulledOut, "")
	assertIDs(t, "pending remove", result.PendingRemove, "")
}

func TestLocalOrderSurvivesWhileRemoteLeavesItsOwnAlone(t *testing.T) {
	result := merged("abc", "abc", "cba")
	assertIDs(t, "order", result.Order, "cba")
	if result.OrderSource != Local {
		t.Errorf("order source = %s, want local", result.OrderSource)
	}
}

// Order is one property, so remote taking it takes all of it.
func TestARemoteReorderWinsTheWholePlaylist(t *testing.T) {
	result := merged("abc", "cba", "bac")
	assertIDs(t, "order", result.Order, "cba")
	if result.OrderSource != Remote {
		t.Errorf("order source = %s, want remote", result.OrderSource)
	}
}

// Otherwise adding one video on a phone would discard the server's order.
func TestAnAdditionAloneIsNotARemoteReorder(t *testing.T) {
	result := merged("abc", "abcg", "cba")
	assertIDs(t, "order", result.Order, "cbag")
	if result.OrderSource != Local {
		t.Errorf("order source = %s, want local", result.OrderSource)
	}
}

func TestARemovalAloneIsNotARemoteReorder(t *testing.T) {
	result := merged("abc", "ac", "cba")
	assertIDs(t, "order", result.Order, "ca")
	if result.OrderSource != Local {
		t.Errorf("order source = %s, want local", result.OrderSource)
	}
}

// Remote wins the order it knows about, and holds no opinion on x.
func TestALocalAdditionSurvivesRemoteTakingTheOrder(t *testing.T) {
	result := merged("abc", "cba", "abxc")
	if result.OrderSource != Remote {
		t.Errorf("order source = %s, want remote", result.OrderSource)
	}
	if !sameMembers(result.Order, "abcx") || !slices.Equal(result.Order[:3], ids("cba")) {
		t.Errorf("order = %q, want c, b, a first and x among them", result.Order)
	}
	assertIDs(t, "pending add", result.PendingAdd, "x")
}

func TestTheSameVideoTwiceIsTwoSlotsNotOne(t *testing.T) {
	result := merged("aba", "aba", "aba")
	assertIDs(t, "order", result.Order, "aba")
	if result.ToPush() {
		t.Errorf("result %+v, want nothing to push", result)
	}
}

func TestDeletingOneOfTwoCopiesDeletesOneOfTwoCopies(t *testing.T) {
	result := merged("aba", "aba", "ab")
	assertIDs(t, "order", result.Order, "ab")
	assertIDs(t, "pending remove", result.PendingRemove, "a")
}

func TestAddingASecondCopyOnYouTubeBringsASecondCopyHere(t *testing.T) {
	result := merged("ab", "aba", "ab")
	assertIDs(t, "order", result.Order, "aba")
	assertIDs(t, "pulled in", result.PulledIn, "a")
}

// One machine removed c and e and pushed, this server deleted b, and a phone
// appended g. Every edit survives, and the base is what makes c and e read as
// deletions on YouTube rather than as additions here waiting to go up.
func TestEveryEditFromThreeSidesSurvives(t *testing.T) {
	result := merged("abcdef", "abdfg", "acdef")
	assertIDs(t, "order", result.Order, "adfg")
	assertIDs(t, "pulled in", result.PulledIn, "g")
	assertIDs(t, "pulled out", result.PulledOut, "ce")
	assertIDs(t, "pending remove", result.PendingRemove, "b")
	assertIDs(t, "pending add", result.PendingAdd, "")
}

// This is what the base prevents. Without one, c and e read as additions here
// and would go back up to YouTube, undoing a deletion made there.
func TestTheSameEditsWithNoBaseResurrectWhatWasDeleted(t *testing.T) {
	result := merged("", "abdfg", "acdef")
	if !sameMembers(result.PendingAdd, "ce") {
		t.Errorf("pending add = %q, want c and e", result.PendingAdd)
	}
}

// The base is written from the remote read, so a deletion here stays pending
// until it is pushed rather than being forgotten by the next merge.
func TestAMergeRunTwiceChangesNothingTheSecondTime(t *testing.T) {
	first := merged("abcdef", "abdfg", "acdef")
	second := Merge(ids("abdfg"), ids("abdfg"), first.Order)
	if !slices.Equal(second.Order, first.Order) {
		t.Errorf("second order = %q, want %q", second.Order, first.Order)
	}
	assertIDs(t, "second pending remove", second.PendingRemove, "b")
	if second.ChangedHere() {
		t.Errorf("second result %+v, want nothing changed here", second)
	}
}
