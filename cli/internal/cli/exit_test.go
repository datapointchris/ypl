package cli

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
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
		{"play", "one", "two"},
		{"play", "--limit", "101"},
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

	// A word one slip from a command is answered with the command, at the root
	// and inside a namespace alike.
	f := newFixture(t, serves(nil))
	for args, meant := range map[[2]string]string{
		{"playslist", "play"}: "playlists",
		{"playlists", "lisy"}: "list",
	} {
		if got := f.run(args[:]...); !slices.Contains(strings.Fields(got.err), meant) {
			t.Errorf("%v said %q, want it to name %q", args, got.err, meant)
		}
	}

	// A command that moved is answered with the line that replaced it, whether
	// it is run or its help is asked for.
	for now, args := range map[string][]string{
		"ypl server status":     {"status"},
		"ypl server syncs list": {"sync", "runs", "list", "--limit", "5"},
	} {
		if got := f.run(args...); got.code != 2 || !strings.Contains(got.err, now) {
			t.Errorf("%v exited %d saying %q, want 2 and `%s`", args, got.code, got.err, now)
		}
		if got := f.run("help", args[0]); !strings.Contains(got.out, now) {
			t.Errorf("`ypl help %s` said %q, want `%s`", args[0], got.out, now)
		}
	}

	// --no-input sits on the verbs that would take the terminal and on no
	// other, and each of them refuses under it before asking anything. The
	// refusal is matched rather than the flag's name, because cobra's own
	// "unknown flag" names it too and also exits 2.
	takesTerminal := map[string][]string{
		"ypl playlists delete": {"playlists", "delete", "sunday-morning"},
		"ypl playlists edit":   {"playlists", "edit", "sunday-morning"},
		"ypl plays delete":     {"plays", "delete", "41"},
	}
	declaring := binding(newRootCommand(&app{}), noInput, nil)
	if len(declaring) != len(takesTerminal) {
		t.Errorf("--no-input is on %v, want exactly the verbs that take the terminal", declaring)
	}
	for _, path := range declaring {
		if _, ok := takesTerminal[strings.Join(append([]string{"ypl"}, path...), " ")]; !ok {
			t.Errorf("--no-input is on %v, which takes no terminal", path)
		}
	}
	for path, args := range takesTerminal {
		terminal := newFixture(t, serves(nil))
		terminal.atTerminal("y\n")
		got := terminal.run(append(args, "--no-input")...)
		if got.code != 2 || !strings.Contains(got.err, "refusing to") || len(terminal.asked) != 0 {
			t.Errorf("%s --no-input at a terminal exited %d after %d requests saying %q, want 2, none, and a refusal",
				path, got.code, len(terminal.asked), got.err)
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
		// The root is the one node that answers bare, with the glance.
		if len(args) == 0 {
			continue
		}
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

	// The not-found hint replaces the text it is handed, so it is handed all
	// of it. A refused edit carries where its buffer was kept beside the
	// server's sentence, and that is the only copy of the rearranging left.
	refused := errors.Join(&api.Refusal{Status: http.StatusNotFound, Code: "not_found", Message: "playlist nope not found"},
		errors.New("the buffer is kept at /tmp/ypl-edit"))
	if subject, ok := notFound(refused); !ok || !strings.Contains(subject, "/tmp/ypl-edit") {
		t.Errorf("the hint went under %q, want every part of the error", subject)
	}
	// Only a reference the server did not recognize is a not-found. Two
	// playlists answering one reference already name both, and a refusal with
	// no sentence never reached the server.
	for _, c := range []struct {
		refusal api.Refusal
		want    bool
	}{
		{api.Refusal{Status: http.StatusBadRequest, Code: "unknown_reference", Message: "names nothing"}, true},
		{api.Refusal{Status: http.StatusBadRequest, Code: "ambiguous_reference", Message: "names two"}, false},
		{api.Refusal{Status: http.StatusNotFound, Code: "not_found"}, false},
		{api.Refusal{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "down"}, false},
	} {
		if _, ok := notFound(&c.refusal); ok != c.want {
			t.Errorf("%+v read as a not-found: %v, want %v", c.refusal, ok, c.want)
		}
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
		{"server", "status", "--json"},
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

	got := f.run("server", "status")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if !strings.Contains(got.err, "Bad Gateway") {
		t.Fatalf("stderr = %q, want the status named", got.err)
	}
}

// binding walks the assembled tree for every command declaring flag, so a
// command added later is covered without anyone remembering to list it.
func binding(cmd *cobra.Command, flag string, path []string) [][]string {
	var found [][]string
	here := path
	if cmd.Name() != "ypl" {
		here = append(append([]string{}, path...), cmd.Name())
	}
	if cmd.Flags().Lookup(flag) != nil {
		found = append(found, here)
	}
	for _, child := range cmd.Commands() {
		if child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		found = append(found, binding(child, flag, here)...)
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
	commands := binding(newRootCommand(&app{}), "json", nil)
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
		{"server", "syncs", "list"},
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
	// The gate reads a hint by its delimiters, so a flag, a placeholder or a
	// hyphen in the verb does not make one invisible to it. A narrower pattern
	// would pass every run below on the one plain hint each happens to carry.
	for sentence, want := range map[string][]string{
		"`ypl status` says what it holds.":                  {"status"},
		"`ypl videos list --json` is the whole library":     {"videos", "list"},
		"`ypl playlists show <playlist>` names one":         {"playlists", "show"},
		"`ypl sync runs list -n 5` shows the newest":        {"sync", "runs", "list"},
		"`ypl playlists refresh-all` re-reads them, a verb": {"playlists", "refresh-all"},
	} {
		found := hinted.FindStringSubmatch(sentence)
		if len(found) < 2 || !slices.Equal(words(found[1]), want) {
			t.Fatalf("the gate read %q as %v, want %v", sentence, found, want)
		}
	}

	said := map[string]bool{}
	exercised := map[string]bool{}
	// Cobra adds `help` as the binary executes, so the tree read here adds it
	// too; a bare `ypl` names it.
	root := func() *cobra.Command {
		tree := newRootCommand(&app{})
		tree.InitDefaultHelpCmd()
		return tree
	}
	// Each invocation is required to produce a hint of its own. A floor across
	// the whole set is satisfied by any one survivor, so dropping the backticks
	// from three of four sentences would leave it green.
	for _, args := range [][]string{
		{"playlists", "list"},
		{"videos", "list", "--artist", "nobody"},
		{"plays", "list"},
		{"server", "syncs", "list"},
		{"next"},
		{"now"},
		{"play", "Empty"},
		{"play"},
		{},
		{"auth", "status"},
		{"auth", "token"},
	} {
		f := newFixture(t, serves(map[string]string{
			"/api/v1/status": `{"library": {"playlists": 0, "videos": 0, "unavailable_videos": 0,
				"enriched_videos": 0, "tracks": 0, "plays": 0}, "last_run": ` + run(1, "failed") + `, "last_ok_run": null}`,
			"/api/v1/playlists":   `[]`,
			"/api/v1/videos":      `[]`,
			"/api/v1/suggestions": `[]`,
			"/api/v1/plays":       `{"data": [], "has_more": false}`,
			"/api/v1/sync/runs":   `{"data": [], "has_more": false}`,
			"/api/v1/playlists/Empty": `{"id": "PLA", "title": "Empty", "privacy": "private", "item_count": 1,
				"enriched_count": 0, "unavailable_count": 1, "synced_ts": null, "items": [
					{"position": 1, "video": {"id": "aaaaaaaaaaa", "title": "A", "channel_title": "One",
						"duration_seconds": 60, "is_unavailable": true}}]}`,
		}))
		// `now` reads a socket and `play` runs a player, and both write their
		// hint before reaching either.
		shortStateDir(t)
		stubMpv(t, 0)
		got := f.run(args...)
		if found, _, err := root().Find(args); err == nil {
			exercised[found.CommandPath()] = true
		}
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

	// A not-found's hints are printed only when a server refuses, so they are
	// read off the tree, where every one of them is, rather than off a run.
	annotated := 0
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, hint := range hinted.FindAllStringSubmatch(cmd.Annotations[goclikit.RecoveryHintsAnnotation], -1) {
			said[hint[1]] = true
			annotated++
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root())
	if annotated == 0 {
		t.Error("no recovery hint on the tree names a command the gate can read")
	}

	// Find returns the deepest command it matched plus the words left over, and
	// no error for a word that named nothing. The leftovers are the whole
	// finding: `ypl playlists refresh` resolves to playlists with "refresh"
	// unconsumed, which is exactly the hint this gate exists to catch.
	// The runs above are a list, and a list is true only when it was written.
	// Every leaf the tree grows is either exercised or named here, so a new
	// command cannot go uncovered in silence. A leaf lands here because it
	// prompts, writes to YouTube, or answers completely and so has nothing to
	// suggest next.
	writesNoHintHere := map[string]bool{
		"ypl auth login": true, "ypl auth logout": true,
		"ypl config example": true, "ypl config path": true,
		"ypl config show": true, "ypl help": true, "ypl update": true,
		"ypl playlists create": true, "ypl playlists delete": true, "ypl playlists edit": true,
		"ypl playlists rename": true, "ypl playlists show": true,
		"ypl plays add": true, "ypl plays delete": true, "ypl plays show": true,
		"ypl server status": true, "ypl videos show": true, "ypl videos sorts": true,
	}
	grown := leaves(root())
	for _, leaf := range grown {
		if !exercised[leaf] && !writesNoHintHere[leaf] {
			t.Errorf("%s is in the tree and no run here reads the hint it writes", leaf)
		}
	}
	// An exemption for a command the tree does not have would exempt it the
	// day somebody adds it, before anyone has read what it suggests.
	for leaf := range writesNoHintHere {
		if !slices.Contains(grown, leaf) {
			t.Errorf("%s is exempted and the tree has no such command", leaf)
		}
	}

	for hint := range said {
		named := words(hint)
		if len(named) == 0 {
			t.Errorf("a command told the reader to run `ypl %s`, which names no command at all", hint)
			continue
		}
		// A hidden command is one that moved, and running it only refuses.
		tree := root()
		found, rest, err := tree.Find(named)
		if err != nil || len(rest) > 0 || found == tree || found.Hidden {
			t.Errorf("a command told the reader to run `ypl %s`, which the tree does not have", hint)
		}
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

// leaves is every command in the tree that runs something, named as cobra spells
// a command path. A namespace is not one: it only shows help. Neither is a
// hidden command, which is one that moved and only refuses.
func leaves(cmd *cobra.Command) []string {
	if len(cmd.Commands()) == 0 {
		return []string{cmd.CommandPath()}
	}
	found := []string{}
	for _, child := range cmd.Commands() {
		if !child.Hidden {
			found = append(found, leaves(child)...)
		}
	}
	return found
}
