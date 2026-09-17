package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// usePad sizes the command column from each row's whole Use string rather than
// from the bare verb. Cobra's own template lists `show`; this lists
// `show <playlist>`, so what a verb takes is on the screen that lists it rather
// than one drill-down further in.
func usePad(cmds []*cobra.Command) int {
	width := 0
	for _, cmd := range cmds {
		if !cmd.IsAvailableCommand() && cmd.Name() != "help" {
			continue
		}
		if n := len(cmd.Use); n > width {
			width = n
		}
	}
	// One past the longest row, so it keeps the two-space gutter the tables
	// this CLI prints use. Cobra's own padding leaves it with one.
	return width + 1
}

// usageTemplate is cobra's own with the three command-list rows rewritten to
// print Use at the computed width. Everything around them is left as cobra
// wrote it, so an upgrade that changes the surrounding structure shows up as a
// diff rather than as a quiet divergence.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{$pad := usePad .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Use $pad}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Use $pad}} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Use $pad}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

func init() {
	cobra.AddTemplateFunc("usePad", usePad)
}

// useUsageTemplate sets the template on root, which every command below it
// inherits.
func useUsageTemplate(root *cobra.Command) {
	root.SetUsageTemplate(strings.TrimPrefix(usageTemplate, "\n"))
}
