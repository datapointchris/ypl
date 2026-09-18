package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/cli/internal/api"
)

const createdPlaylist = `{"id": "PLNEW", "title": "Sunday Morning", "description": "", "privacy": "private",
	"item_count": 0, "unavailable_count": 0, "enriched_count": 0}`

func TestPlaylistsCreateSendsTheTitleAndReportsWhatWasMade(t *testing.T) {
	f := newFixture(t, answers(map[string]answer{
		"POST /api/v1/playlists": {status: http.StatusCreated, body: createdPlaylist},
	}))

	got := asJSON[api.PlaylistSummary](t, f.run("playlists", "create", "Sunday Morning", "--json"))
	if got.ID != "PLNEW" || got.Title != "Sunday Morning" {
		t.Fatalf("created %+v, want the playlist the server made", got)
	}
	sent := f.onlyWrite()
	if sent.Method != http.MethodPost || sent.URL.Path != "/api/v1/playlists" {
		t.Fatalf("sent %s %s, want POST to the collection", sent.Method, sent.URL.Path)
	}
	if strings.TrimSpace(sent.Body) != `{"title":"Sunday Morning"}` {
		t.Fatalf("sent %q, want the title alone", sent.Body)
	}
}

// The new playlist is empty and private, and neither is on the table, so the
// command says both and names what fills it.
func TestPlaylistsCreateSaysItIsEmptyAndNamesWhatFillsIt(t *testing.T) {
	f := newFixture(t, answers(map[string]answer{
		"POST /api/v1/playlists": {status: http.StatusCreated, body: createdPlaylist},
	}))

	got := f.run("playlists", "create", "Sunday Morning")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if !strings.Contains(got.out, "Sunday Morning") || !strings.Contains(got.out, "PLNEW") {
		t.Errorf("stdout = %q, want the playlist and its id", got.out)
	}
	if !strings.Contains(got.err, "empty") || !hinted.MatchString(got.err) {
		t.Errorf("stderr = %q, want it to say the playlist is empty and name what fills it", got.err)
	}
}

func TestPlaylistsRenameSendsTheNewTitleToTheNamedPlaylist(t *testing.T) {
	renamed := `{"id": "PLA", "title": "Sunday Mornings", "description": "", "privacy": "private",
		"item_count": 3, "unavailable_count": 0, "enriched_count": 3}`
	f := newFixture(t, answers(map[string]answer{
		"PATCH /api/v1/playlists/Sunday Morning": {body: renamed},
	}))

	got := asJSON[api.PlaylistSummary](t, f.run("playlists", "rename", "Sunday Morning", "Sunday Mornings", "--json"))
	if got.Title != "Sunday Mornings" {
		t.Fatalf("renamed to %+v, want the new title", got)
	}
	sent := f.onlyWrite()
	if sent.Method != http.MethodPatch {
		t.Fatalf("sent %s, want PATCH", sent.Method)
	}
	if path := sent.URL.EscapedPath(); path != "/api/v1/playlists/Sunday%20Morning" {
		t.Fatalf("asked for %q, want the playlist named as one escaped segment", path)
	}
	if strings.TrimSpace(sent.Body) != `{"title":"Sunday Mornings"}` {
		t.Fatalf("sent %q, want the new title", sent.Body)
	}
}

// Prompting a caller that cannot answer blocks on a stdin that never closes, so
// the refusal comes first — before the token, the config or the network is
// reached — and names what stopped it and the flag that would have answered.
// Somebody at a terminal is asked, by the playlist's own title, and only a yes
// deletes it.
func TestPlaylistsDeleteAsksWhoeverCanAnswerAndRefusesWhereNobodyCan(t *testing.T) {
	f := newFixture(t, answers(map[string]answer{
		"GET /api/v1/playlists/sunday-morning/items": {body: `{"video_ids": ["aaaaaaaaaaa", "bbbbbbbbbbb"]}`},
		"GET /api/v1/playlists/sunday-morning": {body: `{"id": "PLA", "title": "Sunday Morning", "privacy": "private",
			"item_count": 2, "enriched_count": 0, "unavailable_count": 0, "synced_ts": null, "items": []}`},
		"DELETE /api/v1/playlists/sunday-morning": {status: http.StatusNoContent},
	}))

	got := f.run("playlists", "delete", "sunday-morning")
	if got.code != 2 {
		t.Fatalf("exited %d, want 2 — running it again with a flag is the answer", got.code)
	}
	if !strings.Contains(got.err, "--yes") {
		t.Errorf("stderr = %q, want it to name the flag that would have answered", got.err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("it asked the server %d times before refusing", len(f.sent))
	}

	f.atTerminal("")
	got = f.run("playlists", "delete", "sunday-morning", "--no-input")
	if got.code != 2 || !strings.Contains(got.err, "--no-input") || len(f.sent) != 0 {
		t.Fatalf("under --no-input at a terminal, exited %d after %d requests saying %q, want 2, none, and the flag named",
			got.code, len(f.sent), got.err)
	}

	f.atTerminal("n\n")
	got = f.run("playlists", "delete", "sunday-morning")
	if got.code != 1 || len(f.writes()) != 0 {
		t.Fatalf("answered no, exited %d after %d writes, want 1 and none", got.code, len(f.writes()))
	}
	if !strings.Contains(got.err, "Delete Sunday Morning (2 videos) from YouTube?") {
		t.Errorf("asked %q, want the playlist named by its title", got.err)
	}

	f.atTerminal("y\n")
	if got = f.run("playlists", "delete", "sunday-morning"); got.code != 0 {
		t.Fatalf("answered yes, exited %d: %s", got.code, got.err)
	}
	if sent := f.onlyWrite(); sent.Method != http.MethodDelete || sent.URL.Path != "/api/v1/playlists/sunday-morning" {
		t.Errorf("sent %s %s, want the delete of the named playlist", sent.Method, sent.URL.Path)
	}
}

// --yes answers the question, so nothing is read to put in a question nobody is
// being asked.
func TestPlaylistsDeleteWithYesSendsOneDeleteAndReadsNothing(t *testing.T) {
	f := newFixture(t, answers(map[string]answer{
		"DELETE /api/v1/playlists/Sunday Morning": {status: http.StatusNoContent},
	}))

	got := f.run("playlists", "delete", "Sunday Morning", "--yes")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if len(f.sent) != 1 {
		t.Fatalf("made %d requests, want the delete alone: %+v", len(f.sent), f.sent)
	}
	sent := f.onlyWrite()
	if sent.Method != http.MethodDelete || sent.URL.EscapedPath() != "/api/v1/playlists/Sunday%20Morning" {
		t.Fatalf("sent %s %s, want the delete of the named playlist", sent.Method, sent.URL.EscapedPath())
	}
	if got.out != "" {
		t.Errorf("stdout = %q, want a delete to write no data", got.out)
	}
	if !strings.Contains(got.err, "Deleted") {
		t.Errorf("stderr = %q, want it to say what happened", got.err)
	}
}

// The server refuses a reference that only part of a title matches, and that
// refusal is the guard on both verbs that cannot be undone. The client sends the
// reference as typed rather than resolving it to an id first, which would hand a
// loose match to a verb the server resolves exactly.
func TestADeleteSendsTheReferenceAsTypedSoTheServerCanRefuseIt(t *testing.T) {
	f := newFixture(t, refuses(http.StatusNotFound, "unknown_reference", "no playlist is named morning"))

	got := f.run("playlists", "delete", "morning", "--yes")
	if got.code != 1 {
		t.Fatalf("exited %d, want the server's refusal", got.code)
	}
	if !strings.Contains(got.err, "no playlist is named morning") {
		t.Errorf("stderr = %q, want the server's own sentence", got.err)
	}
	if sent := f.onlyWrite(); sent.URL.Path != "/api/v1/playlists/morning" {
		t.Errorf("sent %q, want the reference as typed", sent.URL.Path)
	}
}

// Only a yes approves. EOF is what a closed stdin gives, and reading it as
// approval would delete on the answer nobody gave.
func TestOnlyAYesApprovesAConfirmation(t *testing.T) {
	for _, c := range []struct {
		answered string
		want     bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"", false},
		{"yep\n", false},
	} {
		var asked strings.Builder
		got, err := readConfirmation(&asked, strings.NewReader(c.answered), "Delete it?")
		if err != nil {
			t.Fatalf("readConfirmation(%q): %v", c.answered, err)
		}
		if got != c.want {
			t.Errorf("readConfirmation(%q) = %v, want %v", c.answered, got, c.want)
		}
		if !strings.Contains(asked.String(), "[y/N]") {
			t.Errorf("asked %q, want the default in the question", asked.String())
		}
	}
}
