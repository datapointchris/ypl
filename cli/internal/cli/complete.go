package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// completionTimeout bounds the one request a Tab makes. A shell waiting on a
// completion has frozen the line being typed, so a server that is slow to
// answer is answered with nothing rather than waited on.
const completionTimeout = 2 * time.Second

// completeFlag attaches complete to cmd's flag name. A flag that is not there is
// a mistake in this package rather than anything a caller typed, so it panics,
// and every test builds the tree.
func completeFlag(cmd *cobra.Command, name string, complete cobra.CompletionFunc) {
	if err := cmd.RegisterFlagCompletionFunc(name, complete); err != nil {
		panic(err)
	}
}

// noFiles turns file completion off on every command under cmd that declares
// no completion of its own. Nothing in this tool takes a path as an argument,
// so a Tab falling back to the working directory offers only wrong answers,
// and a command added later would otherwise do it unless someone remembered.
func noFiles(cmd *cobra.Command) {
	if cmd.ValidArgsFunction == nil && len(cmd.ValidArgs) == 0 {
		cmd.ValidArgsFunction = cobra.NoFileCompletions
	}
	for _, child := range cmd.Commands() {
		noFiles(child)
	}
}

// onlyFirst completes a verb's first argument with complete and offers nothing
// after it. `playlists rename <playlist> <title>` takes a playlist and then
// words nothing can guess.
func onlyFirst(complete cobra.CompletionFunc) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return complete(cmd, args, toComplete)
	}
}

// completePlaylists offers every playlist the server holds.
//
// Each is offered as its title slugged, with the title itself beside it, so
// the list a Tab prints reads as titles while what lands on the line needs no
// quoting.
//
// Every failure is answered with no candidates. A completion runs on a
// keystroke in the middle of a line, and no reason for one — no config, no
// login, a server that is down — is worth an error written into it. The same
// command run with Enter says what is wrong.
func (a *app) completePlaylists(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	client, err := a.client(ctx)
	if err != nil {
		cobra.CompDebugln(err.Error(), false)
		return nil, cobra.ShellCompDirectiveError
	}
	playlists, err := client.ListPlaylists(ctx)
	if err != nil {
		cobra.CompDebugln(err.Error(), false)
		return nil, cobra.ShellCompDirectiveError
	}
	return playlistCandidates(playlists), cobra.ShellCompDirectiveNoFileComp
}

// playlistCandidates is each playlist as a completion: its title slugged, then
// the title and how much it holds.
func playlistCandidates(playlists []api.PlaylistSummary) []cobra.Completion {
	shared := map[string]int{}
	for _, playlist := range playlists {
		shared[slug(playlist.Title)]++
	}
	candidates := make([]cobra.Completion, len(playlists))
	for i, playlist := range playlists {
		offered := slug(playlist.Title)
		// Two titles that differ only in case or punctuation slug the same,
		// and the server refuses a reference naming two playlists. The id
		// reaches exactly one.
		if offered == "" || shared[offered] > 1 {
			offered = playlist.ID
		}
		candidates[i] = cobra.CompletionWithDesc(offered,
			fmt.Sprintf("%s, %s", playlist.Title, count(playlist.ItemCount, "video")))
	}
	return candidates
}

// namingPlaylists is err, with every playlist the server holds listed under it
// where err is the server recognizing no playlist by the reference sent.
//
// Listed rather than pointed at: every playlist arrives in one request, the one
// a Tab makes, and an error that can print the valid values owes them. They
// are printed as Tab offers them, so one can be copied back. Where the list
// cannot be read, err is returned as it was.
func namingPlaylists(ctx context.Context, client *api.Client, err error) error {
	if _, ok := notFound(err); !ok {
		return err
	}
	playlists, listErr := client.ListPlaylists(ctx)
	if listErr != nil || len(playlists) == 0 {
		return err
	}
	var listed strings.Builder
	listed.WriteString("The playlists the server holds:\n")
	aligned := tabwriter.NewWriter(&listed, 0, 0, 2, ' ', 0)
	for _, candidate := range playlistCandidates(playlists) {
		offered, described, _ := strings.Cut(candidate, "\t")
		_, _ = fmt.Fprintf(aligned, "  %s\t%s\n", offered, described)
	}
	_ = aligned.Flush()
	return errors.Join(err, errors.New(strings.TrimRight(listed.String(), "\n")))
}

// slug is a title as something a shell takes unquoted: lowercased, with every
// run of anything that is not a letter or a digit standing as one hyphen, and
// letters outside ASCII kept.
//
// It has to equal the slug the server resolves a playlist by, hyphens
// included: the server compares its own slug of the title against its slug of
// the reference, so `deephouse` does not reach Deep House. The two modules
// cannot share the function, so the server writes its answers for a set of
// titles to `testdata/wire/slugs.json` and the completion test holds this one
// to every pair.
func slug(title string) string {
	var built strings.Builder
	pending := false
	for _, r := range title {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			pending = true
			continue
		}
		if pending && built.Len() > 0 {
			built.WriteByte('-')
		}
		pending = false
		built.WriteRune(unicode.ToLower(r))
	}
	return built.String()
}
