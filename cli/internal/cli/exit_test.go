package cli

import (
	"net/http"
	"strings"
	"testing"
)

// Exit 2 is the only answer that tells a caller to try different arguments
// rather than to try again later, so every mistake it can make before a request
// is sent has to reach it.
func TestEveryInvocationMistakeExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"nope"},
		{"playlists", "nope"},
		{"playlists", "show"},
		{"playlists", "show", "one", "two"},
		{"playlists", "list", "--nope"},
		{"videos", "list", "--sort", "sideways"},
		{"videos", "list", "--min-minutes", "120", "--max-minutes", "60"},
		{"plays", "list", "--limit", "0"},
		{"plays", "list", "--limit", "-1"},
		{"plays", "list", "--limit", "half"},
		{"next", "--limit", "101"},
	} {
		f := newFixture(t, serves(nil))
		if got := f.run(args...); got.code != 2 {
			t.Errorf("%v exited %d, want 2: %s%s", args, got.code, got.out, got.err)
		}
	}
}

// A value the parser refuses never reaches the config, the keychain or the
// network, so the caller is told they typed it wrong rather than that something
// is unreachable.
func TestARefusedLimitIsCaughtBeforeAnythingIsAsked(t *testing.T) {
	f := newFixture(t, serves(nil))

	f.run("plays", "list", "--limit", "0")
	if len(f.asked) != 0 {
		t.Fatalf("a refused limit still made %d requests", len(f.asked))
	}
}

// A namespace expects another word after it, so bare shows help and exits 0
// rather than failing at someone walking down the tree a word at a time.
func TestEveryNamespaceShowsHelpWhenGivenNothing(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"playlists"},
		{"videos"},
		{"plays"},
		{"sync"},
		{"sync", "runs"},
		{"auth"},
		{"config"},
	} {
		f := newFixture(t, serves(nil))
		got := f.run(args...)
		if got.code != 0 {
			t.Errorf("%v exited %d, want help and 0", args, got.code)
		}
		if !strings.Contains(got.out, "Usage:") {
			t.Errorf("%v printed no usage: %s", args, got.out)
		}
	}
}

// The server's sentence is what says which playlist was not found, so it
// reaches the terminal rather than being replaced with a status code.
func TestARefusalFromTheServerCarriesItsOwnSentence(t *testing.T) {
	f := newFixture(t, refuses(http.StatusNotFound, "not_found", "playlist nope not found"))

	got := f.run("playlists", "show", "nope")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if !strings.Contains(got.err, "playlist nope not found") {
		t.Fatalf("stderr = %q, want the server's own sentence", got.err)
	}
}

// A token the server will not take is answered by logging in again and never by
// a different request, so the sentence names the command rather than the status.
func TestARejectedTokenPointsAtTheLoginCommand(t *testing.T) {
	f := newFixture(t, refuses(http.StatusUnauthorized, "invalid_token", "the token is not valid"))

	got := f.run("playlists", "list")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if !strings.Contains(got.err, "ypl auth login") {
		t.Fatalf("stderr = %q, want it to name `ypl auth login`", got.err)
	}
}

// A caller reading a non-zero exit is never also holding half a document, so a
// refusal writes nothing at all to stdout.
func TestNothingReachesStdoutWhenACommandRefuses(t *testing.T) {
	f := newFixture(t, refuses(http.StatusBadGateway, "youtube_read_failed", "YouTube did not answer"))

	for _, args := range [][]string{
		{"playlists", "list", "--json"},
		{"videos", "list", "--json"},
		{"status", "--json"},
	} {
		got := f.run(args...)
		if got.out != "" {
			t.Errorf("%v wrote %q to stdout while exiting %d", args, got.out, got.code)
		}
	}
}

// An answer that is not the server's own — a proxy's error page, an identity
// provider's redirect — still has to produce something a caller can act on.
func TestAnAnswerThatIsNotTheServersStillCarriesItsStatus(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	})

	got := f.run("status")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if !strings.Contains(got.err, "Bad Gateway") {
		t.Fatalf("stderr = %q, want the status named", got.err)
	}
}

func TestClockReadsSecondsAsAPersonWritesADuration(t *testing.T) {
	for _, c := range []struct {
		seconds *int64
		want    string
	}{
		{nil, ""},
		{known(0), "0:00"},
		{known(59), "0:59"},
		{known(90), "1:30"},
		{known(3600), "1:00:00"},
		{known(7325), "2:02:05"},
	} {
		if got := clock(c.seconds); got != c.want {
			t.Errorf("clock(%v) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func known(n int64) *int64 { return &n }
