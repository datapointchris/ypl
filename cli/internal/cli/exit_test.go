package cli

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
		{"videos", "list", "--min-minutes", "120", "--max-minutes", "60"},
		{"videos", "list", "--min-minutes", "-5"},
		{"videos", "list", "--max-minutes", "-1"},
		{"videos", "list", "--min-minutes", "153722867280912931"},
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

// A caller can mean no rows — `tail -n 0` and `head -n 0` both print nothing —
// so refusing it would reserve a value somebody could have intended. It needs
// no request to answer.
func TestALimitOfNothingAsksForNothing(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/plays": `{"data": [], "has_more": true}`}))

	got := f.run("plays", "list", "--limit", "0", "--json")
	if got.code != 0 {
		t.Fatalf("exited %d, want 0: %s%s", got.code, got.out, got.err)
	}
	if trimmed := strings.TrimSpace(got.out); trimmed != "[]" {
		t.Errorf("wrote %q, want []", trimmed)
	}
	if len(f.asked) != 0 {
		t.Errorf("a limit of nothing made %d requests", len(f.asked))
	}
}

// The server owns the vocabulary and refuses an order it does not have, naming
// every one it does. Refusing here as well would mean a sort the server gains
// needs a new binary before anyone can use it.
func TestAnUnknownSortIsTheServersToRefuse(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/videos": `[]`}))

	got := f.run("videos", "list", "--sort", "sideways", "--json")
	if got.code != 0 {
		t.Fatalf("exited %d, want the request to be made: %s%s", got.code, got.out, got.err)
	}
	if asked := f.lastAsked().Query().Get("sort"); asked != "sideways" {
		t.Fatalf("sort reached the server as %q, want it sent as typed", asked)
	}
}

// A value the parser refuses never reaches the config, the keychain or the
// network, so the caller is told they typed it wrong rather than that something
// is unreachable.
func TestARefusedLimitIsCaughtBeforeAnythingIsAsked(t *testing.T) {
	f := newFixture(t, serves(nil))

	// Paired with the exit code and the sentence: any pre-request failure makes
	// no requests, so the absence alone cannot tell this one from a typo in the
	// verb.
	got := f.run("plays", "list", "--limit", "-1")
	if got.code != 2 {
		t.Fatalf("exited %d, want 2", got.code)
	}
	if !strings.Contains(got.err, "negative") {
		t.Errorf("stderr = %q, want it to name what was wrong with the value", got.err)
	}
	if len(f.asked) != 0 {
		t.Fatalf("a refused limit still made %d requests", len(f.asked))
	}
}

// namespaces walks the assembled tree for every node expecting another word
// after it. Listing them instead would be complete today and silently short the
// day a command is added, which is the population this test is named for.
func namespaces(cmd *cobra.Command, path []string) [][]string {
	here := path
	if cmd.Name() != "ypl" {
		here = append(append([]string{}, path...), cmd.Name())
	}
	if !cmd.HasSubCommands() {
		return nil
	}
	found := [][]string{here}
	for _, child := range cmd.Commands() {
		if child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		found = append(found, namespaces(child, here)...)
	}
	return found
}

// A namespace expects another word after it, so bare shows help and exits 0
// rather than failing at someone walking down the tree a word at a time.
func TestEveryNamespaceShowsHelpWhenGivenNothing(t *testing.T) {
	found := namespaces(newRootCommand(&app{}), nil)
	if len(found) < 8 {
		t.Fatalf("walked %d namespaces, want every node with subcommands", len(found))
	}
	for _, args := range found {
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

// jsonCapable walks the assembled tree for every leaf binding --json, so a
// command added later is covered without anyone remembering to list it.
func jsonCapable(cmd *cobra.Command, path []string) [][]string {
	var found [][]string
	here := path
	if cmd.Name() != "ypl" {
		here = append(append([]string{}, path...), cmd.Name())
	}
	if cmd.Flags().Lookup("json") != nil {
		found = append(found, here)
	}
	for _, child := range cmd.Commands() {
		if child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		found = append(found, jsonCapable(child, here)...)
	}
	return found
}

// The exit code is the machine contract and the rendering is not, so a flag
// that only picks a rendering never moves it. The --json reader is the one who
// cannot see the sentence on stderr, so it is the one most dependent on the
// code being right.
func TestTheRenderingNeverDecidesTheExitCode(t *testing.T) {
	empty := map[string]string{
		"/api/v1/playlists":   `[]`,
		"/api/v1/videos":      `[]`,
		"/api/v1/suggestions": `[]`,
		"/api/v1/plays":       `{"data": [], "has_more": false}`,
		"/api/v1/sync/runs":   `{"data": [], "has_more": false}`,
		"/api/v1/status":      `{"library": {}, "last_run": null, "last_ok_run": null}`,
	}
	commands := jsonCapable(newRootCommand(&app{}), nil)
	if len(commands) < 8 {
		t.Fatalf("found %d commands taking --json, want every leaf that binds it", len(commands))
	}
	for _, args := range commands {
		plain := newFixture(t, serves(empty)).run(args...)
		asJSON := newFixture(t, serves(empty)).run(append(append([]string{}, args...), "--json")...)
		if plain.code != asJSON.code {
			t.Errorf("%v exits %d and --json exits %d", args, plain.code, asJSON.code)
		}
	}
}

// A read with nothing in it is the moment someone is least able to tell an
// empty answer from a broken command, so it says which it was and what to run.
func TestAReadWithNothingInItSaysSoAndNamesWhatToRunNext(t *testing.T) {
	for _, args := range [][]string{
		{"playlists", "list"},
		{"videos", "list", "--artist", "nobody"},
		{"plays", "list"},
		{"sync", "runs", "list"},
	} {
		f := newFixture(t, serves(map[string]string{
			"/api/v1/playlists": `[]`,
			"/api/v1/videos":    `[]`,
			"/api/v1/plays":     `{"data": [], "has_more": false}`,
			"/api/v1/sync/runs": `{"data": [], "has_more": false}`,
		}))
		got := f.run(args...)
		switch {
		case got.code != 0:
			t.Errorf("%v exited %d, want 0 — an empty answer is data", args, got.code)
		case got.out != "":
			t.Errorf("%v wrote %q to stdout, want the sentence on stderr", args, got.out)
		case strings.TrimSpace(got.err) == "":
			t.Errorf("%v printed nothing at all, which reads as a broken command", args)
		}
	}
}

// hinted is every `ypl ...` a command wrote, which is what it told the reader to
// run next.
//
// Bounded by the backticks that delimit a hint rather than by the letters
// today's hints happen to use. An alphabet bound cannot see a hint carrying a
// flag, a placeholder or a hyphen — and a hyphenated verb the tree does not
// have is exactly what this gate exists to catch.
var hinted = regexp.MustCompile("`ypl ([^`]+)`")

// words is the command part of a hint: everything before the first flag or
// placeholder, which are arguments rather than names in the tree.
func words(hint string) []string {
	var named []string
	for _, word := range strings.Fields(hint) {
		if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "<") || strings.HasPrefix(word, "[") {
			break
		}
		named = append(named, word)
	}
	return named
}

// A hint naming a command the tool does not have is worse than no hint, because
// it reads as authoritative and spends the attention the reader had left. The
// walk is against the assembled tree rather than one built here, since a gate
// reading a tree the binary never assembles passes while the binary is broken.
func TestEverySuggestedCommandExists(t *testing.T) {
	said := map[string]bool{}
	// Each invocation is required to produce a hint of its own. A floor across
	// the whole set is satisfied by any one survivor, so dropping the backticks
	// from three of four sentences would leave it green.
	for _, args := range [][]string{
		{"playlists", "list"},
		{"videos", "list", "--artist", "nobody"},
		{"plays", "list"},
		{"sync", "runs", "list"},
		{"next"},
		{"auth", "status"},
		{"auth", "token"},
	} {
		f := newFixture(t, serves(map[string]string{
			"/api/v1/playlists":   `[]`,
			"/api/v1/videos":      `[]`,
			"/api/v1/suggestions": `[]`,
			"/api/v1/plays":       `{"data": [], "has_more": false}`,
			"/api/v1/sync/runs":   `{"data": [], "has_more": false}`,
		}))
		got := f.run(args...)
		here := hinted.FindAllStringSubmatch(got.out+got.err, -1)
		if len(here) == 0 {
			t.Errorf("%v named no command to run next", args)
		}
		for _, found := range here {
			said[found[1]] = true
		}
	}

	// The unconfigured refusal is the other hint-producing path, and the one a
	// first run meets.
	unconfigured := newFixture(t, serves(nil))
	t.Setenv("YPL_API_BASE", "")
	t.Setenv("YPL_OIDC_ISSUER", "")
	refusal := unconfigured.run("config", "show")
	found := hinted.FindAllStringSubmatch(refusal.out+refusal.err, -1)
	if len(found) == 0 {
		t.Error("the unconfigured refusal named no command that would configure it")
	}
	for _, one := range found {
		said[one[1]] = true
	}

	// Find returns the deepest command it matched plus the words left over, and
	// no error for a word that named nothing. The leftovers are the whole
	// finding: `ypl playlists refresh` resolves to playlists with "refresh"
	// unconsumed, which is exactly the hint this gate exists to catch.
	root := newRootCommand(&app{})
	for hint := range said {
		named := words(hint)
		if len(named) == 0 {
			t.Errorf("a command told the reader to run `ypl %s`, which names no command at all", hint)
			continue
		}
		found, rest, err := root.Find(named)
		if err != nil || len(rest) > 0 || found == root {
			t.Errorf("a command told the reader to run `ypl %s`, which the tree does not have", hint)
		}
	}
}

// The gate reads a hint by its delimiters, so a flag, a placeholder or a hyphen
// in the verb does not make one invisible to it.
func TestTheHintGateReadsEveryShapeOfHint(t *testing.T) {
	for _, c := range []struct {
		sentence string
		want     []string
	}{
		{"`ypl status` says what it holds.", []string{"status"}},
		{"`ypl videos list --json` is the whole library", []string{"videos", "list"}},
		{"`ypl playlists show <playlist>` names one", []string{"playlists", "show"}},
		{"`ypl sync runs list -n 5` shows the newest", []string{"sync", "runs", "list"}},
	} {
		found := hinted.FindStringSubmatch(c.sentence)
		if found == nil {
			t.Errorf("%q matched nothing", c.sentence)
			continue
		}
		if got := words(found[1]); !slices.Equal(got, c.want) {
			t.Errorf("%q named %v, want %v", c.sentence, got, c.want)
		}
	}
	// A hyphenated verb the tree does not have is the shape the gate is for.
	if found := hinted.FindStringSubmatch("`ypl playlists refresh-all` re-reads them"); found == nil {
		t.Error("a hyphenated verb was invisible to the gate")
	}
}

// A 401 is answered by logging in again, and the sentence saying so is one the
// gate above has to be able to see.
func TestTheRejectedTokenHintIsAmongTheCheckedOnes(t *testing.T) {
	f := newFixture(t, refuses(http.StatusUnauthorized, "invalid_token", "no"))

	if got := f.run("playlists", "list"); !hinted.MatchString(got.err) {
		t.Fatalf("stderr = %q, want a `ypl ...` the gate can check", got.err)
	}
}

// "1 videos" is a rendering fault a reader notices, and one noticed costs the
// rest of the line its credibility.
func TestCountNamesOneThingSingly(t *testing.T) {
	for _, c := range []struct {
		n     int64
		thing string
		want  string
	}{
		{0, "video", "0 videos"},
		{1, "video", "1 video"},
		{2, "video", "2 videos"},
		{1, "playlist", "1 playlist"},
		{11, "track", "11 tracks"},
	} {
		if got := count(c.n, c.thing); got != c.want {
			t.Errorf("count(%d, %q) = %q, want %q", c.n, c.thing, got, c.want)
		}
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
