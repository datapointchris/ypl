package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// emitJSON writes v to out as the machine rendering. It is the only thing any
// command writes to stdout besides its table, because anything else on that
// stream reaches a caller's parser as malformed JSON rather than as the note it
// was meant to be.
func emitJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// column is one column of a table: its heading, and whether its cells may be
// cut short to fit the terminal.
type column struct {
	heading string
	// prose marks titles and names, which a reader still recognizes cut short.
	// An id, a number or a time is what the next command is given or what a
	// reader compares, so it is never cut.
	prose bool
}

// whole is a column never cut; prose is one cut before a row wraps.
func whole(heading string) column { return column{heading: heading} }
func prose(heading string) column { return column{heading: heading, prose: true} }

// shortestProse is the narrowest a prose column is cut to. Below it a title no
// longer says which mix it is, and a wrapped row reads better than that.
const shortestProse = 12

// gutter is the space between two columns.
const gutter = "  "

// table writes rows under the columns' headings, padded by the width each cell
// takes on a screen rather than by its bytes or runes, since a title can hold
// characters two cells wide.
//
// width is the terminal's, and 0 for anything else. At a terminal the widest
// prose column is cut, with an ellipsis, until a row fits. Anything else gets
// every character, since what reads it is grep or a file rather than a person.
// A table with no prose column has nothing to cut and is given 0.
//
// No rows writes nothing, so a caller sees an empty answer rather than a header
// for one.
func table(out io.Writer, width int, columns []column, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(columns))
	for i, c := range columns {
		widths[i] = runewidth.StringWidth(c.heading)
		for _, row := range rows {
			widths[i] = max(widths[i], runewidth.StringWidth(row[i]))
		}
	}
	if width > 0 {
		fit(widths, columns, width)
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i > 0 {
				b.WriteString(gutter)
			}
			cell = runewidth.Truncate(cell, widths[i], "…")
			if i < len(cells)-1 {
				cell = runewidth.FillRight(cell, widths[i])
			}
			b.WriteString(cell)
		}
		_, _ = fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	}
	headings := make([]string, len(columns))
	for i, c := range columns {
		headings[i] = c.heading
	}
	line(headings)
	for _, row := range rows {
		line(row)
	}
}

// fit narrows the widest prose column a cell at a time until a row fits width,
// so two long columns end up cut to about the same width rather than one
// losing everything. It stops at shortestProse, or at a prose column's heading
// where that is wider, and a row that still does not fit wraps.
func fit(widths []int, columns []column, width int) {
	total := len(gutter) * (len(widths) - 1)
	for _, w := range widths {
		total += w
	}
	for total > width {
		widest := -1
		for i, c := range columns {
			floor := max(shortestProse, runewidth.StringWidth(c.heading))
			if c.prose && widths[i] > floor && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			return
		}
		widths[widest]--
		total--
	}
}

// widthOf is how many cells wide the terminal out writes to is, and 0 when out
// is not a terminal.
func widthOf(out io.Writer) int {
	file, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return 0
	}
	return width
}

// nothing says a read found no rows, and names the command that widens it. It
// writes to stderr, so a --json caller's stdout stays one parsable document and
// a person is not left unable to tell an empty answer from a broken command.
func nothing(cmd *cobra.Command, sentence string) {
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), sentence)
}

// count is n things, named singly or plurally. "1 videos" reads as a rendering
// fault, and a reader who notices one stops trusting the rest of the line.
func count(n int64, thing string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, thing)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// clock is a number of seconds as h:mm:ss, or m:ss under an hour. A duration
// the server does not know is an empty cell rather than a zero, since a mix of
// unknown length is not a mix of no length.
func clock(seconds *int64) string {
	if seconds == nil {
		return ""
	}
	d := time.Duration(*seconds) * time.Second
	if h := int64(d.Hours()); h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, int64(d.Minutes())%60, *seconds%60)
	}
	return fmt.Sprintf("%d:%02d", int64(d.Minutes()), *seconds%60)
}

// text is a pointer's string, and "" for one the server does not hold.
func text(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
