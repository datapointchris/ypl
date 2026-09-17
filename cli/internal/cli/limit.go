package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

// rowCount is a --limit whose parser refuses a count no read can mean. pflag
// carries no minimum of its own, and an int flag without one takes --limit=-1
// and quietly asks for a negative number of rows.
//
// The refusal being in the parser is the other half: a rejected value never
// reaches the config, the keychain or the network, so the caller is told they
// typed it wrong rather than that the server is unreachable.
type rowCount struct {
	n *int
	// most is the largest count this read can ask for, and 0 where the read
	// pages and so has no ceiling.
	most int
}

func (c rowCount) String() string { return strconv.Itoa(*c.n) }

func (c rowCount) Type() string { return "int" }

func (c rowCount) Set(raw string) error {
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a whole number", raw)
	case n < 1:
		return fmt.Errorf("a limit is at least 1, and %d asks for no rows at all", n)
	case c.most > 0 && n > c.most:
		return fmt.Errorf("at most %d can be asked for here, and this asks for %d", c.most, n)
	}
	*c.n = n
	return nil
}

// addLimit binds --limit/-n to n, refusing anything below one and anything
// above most. A most of 0 is a read that pages, which has no ceiling.
func addLimit(cmd *cobra.Command, n *int, most int, help string) {
	cmd.Flags().VarP(rowCount{n: n, most: most}, "limit", "n", help)
}

// addJSON binds --json to asJSON.
func addJSON(cmd *cobra.Command, asJSON *bool, what string) {
	cmd.Flags().BoolVar(asJSON, "json", false, "Write "+what+" to stdout as JSON")
}
