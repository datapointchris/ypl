package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
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

// table writes rows under headings, padded into columns. A row with no rows
// under it writes nothing, so a caller sees an empty answer rather than a
// header for one.
func table(out io.Writer, headings []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(headings, "\t"))
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	_ = w.Flush()
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
