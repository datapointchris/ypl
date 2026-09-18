package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The three videos an edit works over, as the server holds the playlist and its
// order.
const (
	first  = "dQw4w9WgXcQ"
	second = "aBcDeFgHiJk"
	third  = "zYxWvUtSrQp"
)

const editedPlaylist = `{"id": "PLA", "title": "Sunday Morning", "description": "", "privacy": "private",
	"item_count": 3, "unavailable_count": 0, "enriched_count": 3, "items": [
		{"id": "IT1", "position": 0, "video": {"id": "dQw4w9WgXcQ", "title": "A Six Hour Mix", "channel_title": "Some Channel",
			"duration_seconds": 21600, "upload_date": null, "is_unavailable": false, "enriched_ts": null, "track_count": 40}},
		{"id": "IT2", "position": 1, "video": {"id": "aBcDeFgHiJk", "title": "Deep House", "channel_title": "Another",
			"duration_seconds": 4350, "upload_date": null, "is_unavailable": false, "enriched_ts": null, "track_count": 12}},
		{"id": "IT3", "position": 2, "video": {"id": "zYxWvUtSrQp", "title": "Late Night", "channel_title": "A Third",
			"duration_seconds": null, "upload_date": null, "is_unavailable": false, "enriched_ts": null, "track_count": 0}}
	]}`

// editing is the server a `playlists edit` reads: the playlist for its titles,
// then the order it is made of, with the revision an edit of it names.
func editing(order string, put answer) map[string]answer {
	return map[string]answer{
		"GET /api/v1/playlists/Sunday Morning":       {body: editedPlaylist},
		"GET /api/v1/playlists/Sunday Morning/items": {body: order, header: map[string]string{"ETag": `"3"`}},
		"PUT /api/v1/playlists/Sunday Morning/items": put,
	}
}

const heldOrder = `{"video_ids": ["dQw4w9WgXcQ", "aBcDeFgHiJk", "zYxWvUtSrQp"]}`

// The revision says which order was edited, and an edit sent without it is
// refused for a precondition the person never chose to leave out.
func TestAnEditSendsTheNewOrderWithTheRevisionItWasReadAt(t *testing.T) {
	reordered := `{"video_ids": ["zYxWvUtSrQp", "dQw4w9WgXcQ", "aBcDeFgHiJk"]}`
	f := newFixture(t, answers(editing(heldOrder, answer{body: reordered, header: map[string]string{"ETag": `"4"`}})))
	f.pipe(third + "\n" + first + "\n" + second + "\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	sent := f.onlyWrite()
	if sent.Method != http.MethodPut {
		t.Fatalf("sent %s, want PUT", sent.Method)
	}
	if match := sent.Header.Get("If-Match"); match != `"3"` {
		t.Errorf("If-Match is %q, want the ETag the order was read at", match)
	}
	var body struct {
		VideoIDs []string `json:"video_ids"`
	}
	if err := json.Unmarshal([]byte(sent.Body), &body); err != nil {
		t.Fatalf("the body %q does not decode: %v", sent.Body, err)
	}
	if want := []string{third, first, second}; !slices.Equal(body.VideoIDs, want) {
		t.Fatalf("sent %v, want %v", body.VideoIDs, want)
	}
}

// An empty buffer aborts rather than emptying the playlist, which is what `git
// rebase -i` does and what somebody who deleted every line by accident needs it
// to do.
func TestAnEmptyBufferAbortsAndSendsNothing(t *testing.T) {
	f := newFixture(t, answers(editing(heldOrder, answer{status: http.StatusOK, body: heldOrder})))
	f.pipe("# every line deleted\n\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("an empty buffer sent %d writes: %+v", len(writes), writes)
	}
	if !strings.Contains(got.err, "aborts") {
		t.Errorf("stderr = %q, want it to say an empty buffer aborts", got.err)
	}
}

// Rearranging the lines back to where they were is the same request as never
// having touched them, and neither is worth a write.
func TestABufferAskingForTheOrderItAlreadyHasSendsNothing(t *testing.T) {
	f := newFixture(t, answers(editing(heldOrder, answer{status: http.StatusOK, body: heldOrder})))
	f.pipe(first + "  Some Channel - A Six Hour Mix\n" + second + "\n" + third + "\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("an unchanged buffer sent %d writes: %+v", len(writes), writes)
	}
	if !strings.Contains(got.err, "unchanged") {
		t.Errorf("stderr = %q, want it to say nothing changed", got.err)
	}
}

// A line of prose is a mistake in what was typed, so it exits 2 and the playlist
// is left as it was.
func TestALineThatNamesNoVideoIsAMistakeAndNothingIsSent(t *testing.T) {
	f := newFixture(t, answers(editing(heldOrder, answer{status: http.StatusOK, body: heldOrder})))
	f.pipe(first + "\nremember to add the other one\n" + second + "\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 2 {
		t.Fatalf("exited %d, want 2", got.code)
	}
	if !strings.Contains(got.err, "line 2") {
		t.Errorf("stderr = %q, want it to name the line", got.err)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("a refused buffer sent %d writes: %+v", len(writes), writes)
	}
}

// A URL pasted from a browser is how a video gets added, and the id inside it is
// what reaches the server.
func TestAPastedURLIsSentAsItsVideoID(t *testing.T) {
	added := `{"video_ids": ["dQw4w9WgXcQ", "aBcDeFgHiJk", "zYxWvUtSrQp", "nEwVideoIdX"]}`
	f := newFixture(t, answers(editing(heldOrder, answer{body: added})))
	f.pipe(first + "\n" + second + "\n" + third + "\nhttps://www.youtube.com/watch?v=nEwVideoIdX&list=PLabc\n")

	got := f.run("playlists", "edit", "Sunday Morning", "--json")
	result := asJSON[edited](t, got)
	if !slices.Equal(result.Added, []string{"nEwVideoIdX"}) {
		t.Fatalf("added %v, want the id inside the URL", result.Added)
	}
	if !strings.Contains(f.onlyWrite().Body, "nEwVideoIdX") {
		t.Fatalf("sent %q, want the id rather than the address", f.onlyWrite().Body)
	}
}

// Removing a video shifts everything below it, so comparing the two orders whole
// would report every removal as a reordering as well.
func TestARemovalAloneIsNotReportedAsAReordering(t *testing.T) {
	without := `{"video_ids": ["dQw4w9WgXcQ", "zYxWvUtSrQp"]}`
	f := newFixture(t, answers(editing(heldOrder, answer{body: without})))
	f.pipe(first + "\n" + third + "\n")

	result := asJSON[edited](t, f.run("playlists", "edit", "Sunday Morning", "--json"))
	if !slices.Equal(result.Removed, []string{second}) {
		t.Errorf("removed %v, want the one line that was deleted", result.Removed)
	}
	if len(result.Added) != 0 {
		t.Errorf("added %v, want nothing", result.Added)
	}
	if result.Reordered {
		t.Error("reported a reordering, and what survived is in the order it was in")
	}
	if result.ItemCount != 2 {
		t.Errorf("counted %d videos, want what the playlist holds now", result.ItemCount)
	}

	// The same edit rendered. One removal and two left is the one sentence that
	// carries a singular and a plural together, so it is where a renderer that
	// stopped naming one thing singly is caught.
	f = newFixture(t, answers(editing(heldOrder, answer{body: without})))
	f.pipe(first + "\n" + third + "\n")
	if plain := f.run("playlists", "edit", "Sunday Morning"); !strings.Contains(plain.out, "1 video removed. It now holds 2 videos.") {
		t.Errorf("printed %q, want one video removed and two held", plain.out)
	}
}

// The lists are there at every size, so a caller filtering the JSON writes one
// filter and no null guard.
func TestAnEditThatChangedNothingStillAnswersAWholeDocument(t *testing.T) {
	f := newFixture(t, answers(editing(heldOrder, answer{status: http.StatusOK, body: heldOrder})))
	f.pipe("")

	got := f.run("playlists", "edit", "Sunday Morning", "--json")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if strings.Contains(got.out, "null") {
		t.Errorf("stdout = %q, want empty lists rather than nulls", got.out)
	}
	result := asJSON[edited](t, got)
	if result.Title != "Sunday Morning" || result.ItemCount != 3 {
		t.Errorf("answered %+v, want the playlist as it stands", result)
	}
}

// keptBuffer finds the path a refusal said it kept the buffer at.
var keptBuffer = regexp.MustCompile(`(/\S+\.ypl)`)

// A refusal arrives after the editor has closed, so without this the
// rearranging is gone and the only way back is to do it again.
func TestARefusedEditKeepsTheBufferAndSaysWhere(t *testing.T) {
	refusal := `{"error": "the order of Sunday Morning no longer has the ETag If-Match names", "code": "precondition_failed"}`
	f := newFixture(t, answers(editing(heldOrder, answer{status: http.StatusPreconditionFailed, body: refusal})))
	f.pipe(third + "\n" + first + "\n" + second + "\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 1 {
		t.Fatalf("exited %d, want the refusal", got.code)
	}
	if !strings.Contains(got.err, "no longer has the ETag") {
		t.Errorf("stderr = %q, want the server's own sentence", got.err)
	}
	found := keptBuffer.FindStringSubmatch(got.err)
	if found == nil {
		t.Fatalf("stderr = %q, want it to name where the buffer was kept", got.err)
	}
	t.Cleanup(func() { _ = os.Remove(found[1]) })
	held, err := os.ReadFile(found[1])
	if err != nil {
		t.Fatalf("the buffer it named cannot be read: %v", err)
	}
	if !strings.Contains(string(held), third) {
		t.Errorf("the kept buffer is %q, want what was edited", held)
	}
}

// An edit names the order it edited, so an order that arrived without a revision
// is one nothing can be edited from. Saying that here names the cause, where
// sending the edit anyway is refused for a precondition nobody chose to omit.
func TestAnOrderWithNoRevisionIsRefusedBeforeAnythingIsEdited(t *testing.T) {
	routes := editing(heldOrder, answer{status: http.StatusOK, body: heldOrder})
	routes["GET /api/v1/playlists/Sunday Morning/items"] = answer{body: heldOrder}
	f := newFixture(t, answers(routes))
	f.pipe(third + "\n" + first + "\n" + second + "\n")

	got := f.run("playlists", "edit", "Sunday Morning")
	if got.code != 1 {
		t.Fatalf("exited %d, want the read to have been refused", got.code)
	}
	if !strings.Contains(got.err, "ETag") {
		t.Errorf("stderr = %q, want it to name what the order arrived without", got.err)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("it edited anyway, %d times: %+v", len(writes), writes)
	}
}

// A pipe is not a prompt, so --no-input has nothing to forbid about one.
func TestAPipedBufferIsAppliedWithNoInput(t *testing.T) {
	reordered := `{"video_ids": ["zYxWvUtSrQp", "dQw4w9WgXcQ", "aBcDeFgHiJk"]}`
	f := newFixture(t, answers(editing(heldOrder, answer{body: reordered})))
	f.pipe(third + "\n" + first + "\n" + second + "\n")

	got := f.run("playlists", "edit", "Sunday Morning", "--no-input")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if sent := f.onlyWrite(); sent.Method != http.MethodPut {
		t.Fatalf("sent %s, want the edit to have been applied", sent.Method)
	}
}
