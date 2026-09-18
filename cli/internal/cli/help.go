package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A help screen says what the command is, then lists every command a caller
// can run beneath it, spelled from the screen's own command down to the leaf
// with its arguments, then its examples and flags.
//
// Cobra's own screen names only the next word down, so a command three levels
// down is three screens away, each ending in `[command]`. Listing the leaves
// of the whole subtree puts every command on the root and each namespace's
// share of them on that namespace. A row starts at the word after the screen's
// own command, as cobra's rows do, so a reader of help screens that matches a
// row's first word against the command's children still finds them.
//
// The renderer replaces cobra's help and usage templates rather than swapping
// their command rows. Cobra's template gives a namespace a
// `Usage: <cmd> [command]` block, which a first-time user reads as the answer,
// and puts the examples above the commands. This screen has no such block and
// lists the commands first.

func init() {
	// Every list prints in the order the tree adds it, which is the order a
	// command is reached for: play before next, list before delete.
	// Alphabetical puts next above play and create above list.
	cobra.EnableCommandSorting = false
}

// useHelp renders every screen in the tree, help and usage alike.
func useHelp(root *cobra.Command) {
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { writeHelp(cmd.OutOrStdout(), cmd) })
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		writeHelp(cmd.OutOrStderr(), cmd)
		return nil
	})
}

// writeHelp writes cmd's screen to out.
func writeHelp(out io.Writer, cmd *cobra.Command) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", cmd.CommandPath(), cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" {
		fmt.Fprintf(&b, "\n%s\n", long)
	}
	sections := commandSections(cmd)
	flags := flagUsages(cmd)
	if len(sections) == 0 {
		// Cobra counts --help when it decides whether to add [flags], so a
		// command taking no other flag would promise some and list none.
		use := cmd.UseLine()
		if flags == "" {
			use = strings.TrimSuffix(use, " [flags]")
		}
		fmt.Fprintf(&b, "\nUsage:\n  %s\n", use)
	}
	// Each section sizes its own column, so one long command line does not
	// push every description past the width of a terminal.
	for _, s := range sections {
		width := 0
		for _, r := range s.rows {
			width = max(width, len(r.line))
		}
		fmt.Fprintf(&b, "\n%s:\n", s.title)
		for _, r := range s.rows {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, r.line, r.short)
		}
	}
	if cmd.HasExample() {
		fmt.Fprintf(&b, "\nExamples:\n%s\n", strings.TrimRight(cmd.Example, "\n"))
	}
	if flags != "" {
		fmt.Fprintf(&b, "\nFlags:\n%s\n", flags)
	}
	if len(sections) > 0 {
		fmt.Fprintf(&b, "\n`%s <command> --help` shows a command's flags and examples.\n", cmd.CommandPath())
	}
	_, _ = io.WriteString(out, b.String())
}

// row is one command a caller can run, and what it does.
type row struct{ line, short string }

// section is a titled run of rows.
type section struct {
	title string
	rows  []row
}

// commandSections is every command beneath cmd, under cmd's own groups where
// it has them. A namespace's commands sit under the group the namespace is in.
// Every title ends in "commands", as cobra's own heading does.
func commandSections(cmd *cobra.Command) []section {
	children := typeable(cmd)
	if len(children) == 0 {
		return nil
	}
	groups := cmd.Groups()
	if len(groups) == 0 {
		return []section{{title: "Commands", rows: leafRows(cmd, children)}}
	}
	var sections []section
	for _, group := range groups {
		var in []*cobra.Command
		for _, child := range children {
			if child.GroupID == group.ID {
				in = append(in, child)
			}
		}
		if len(in) > 0 {
			sections = append(sections, section{title: strings.TrimSuffix(group.Title, ":"), rows: leafRows(cmd, in)})
		}
	}
	var rest []*cobra.Command
	for _, child := range children {
		if child.GroupID == "" {
			rest = append(rest, child)
		}
	}
	if len(rest) > 0 {
		sections = append(sections, section{title: "Other commands", rows: leafRows(cmd, rest)})
	}
	return sections
}

// typeable is cmd's children a caller can run. help is left out, since
// --help on any command is the same thing, and so is a hidden command.
func typeable(cmd *cobra.Command) []*cobra.Command {
	var children []*cobra.Command
	for _, child := range cmd.Commands() {
		if child.IsAvailableCommand() && child.Name() != "help" {
			children = append(children, child)
		}
	}
	return children
}

// leafRows is every runnable command at or beneath cmds, each spelled from the
// word after screen's command down to the leaf, arguments included.
func leafRows(screen *cobra.Command, cmds []*cobra.Command) []row {
	var rows []row
	for _, cmd := range cmds {
		if children := typeable(cmd); len(children) > 0 {
			rows = append(rows, leafRows(screen, children)...)
			continue
		}
		line := strings.TrimPrefix(cmd.Parent().CommandPath()+" "+cmd.Use, screen.CommandPath()+" ")
		rows = append(rows, row{line: line, short: cmd.Short})
	}
	return rows
}

// helpWidth is the column a flag's description wraps at, so a screen reads in a
// split pane.
const helpWidth = 80

// flagUsages is every flag cmd takes, its own and those it inherits, without
// --help, which every command has.
func flagUsages(cmd *cobra.Command) string {
	set := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	add := func(flag *pflag.Flag) {
		if flag.Name != "help" && !flag.Hidden && set.Lookup(flag.Name) == nil {
			set.AddFlag(flag)
		}
	}
	cmd.LocalFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	return strings.TrimRight(set.FlagUsagesWrapped(helpWidth), "\n")
}
