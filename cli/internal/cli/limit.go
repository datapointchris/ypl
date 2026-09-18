package cli

import (
	"fmt"
	"math"
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

// Set floors at zero rather than at one. A caller can mean no rows — `tail -n 0`
// and `head -n 0` both print nothing — so refusing it reserves a value somebody
// could have intended. Only a negative count is unmeanable.
func (c rowCount) Set(raw string) error {
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a whole number", raw)
	case n < 0:
		return fmt.Errorf("a number of rows cannot be negative; the smallest is 0")
	case c.most > 0 && n > c.most:
		return fmt.Errorf("at most %d can be asked for here, and this asks for %d", c.most, n)
	}
	*c.n = n
	return nil
}

// minutes is a duration bound in whole minutes, floored at zero. Absence is
// carried by the flag's own Changed rather than by a negative, which pflag
// renders into help as `(default -1)` and a reader cannot tell from a bound.
type minutes struct{ n *int64 }

func (m minutes) String() string { return strconv.FormatInt(*m.n, 10) }

func (m minutes) Type() string { return "int" }

func (m minutes) Set(raw string) error {
	n, err := strconv.ParseInt(raw, 10, 64)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a whole number of minutes", raw)
	case n < 0:
		return fmt.Errorf("a duration cannot be negative; the shortest is 0")
	case n > maxMinutes:
		return fmt.Errorf("at most %d minutes can be asked for, and this asks for %d", maxMinutes, n)
	}
	*m.n = n
	return nil
}

// maxMinutes is the largest bound that still converts to seconds inside an
// int64. Without a ceiling the multiplication wraps, and a wrapped bound is
// negative, which the client then spells as no bound at all.
const maxMinutes = math.MaxInt64 / 60

// addMinutes binds a duration bound to n, refusing a negative and anything the
// conversion to seconds could not hold.
func addMinutes(cmd *cobra.Command, name string, n *int64, help string) {
	cmd.Flags().Var(minutes{n: n}, name, help)
}

// addLimit binds --limit/-n to n, refusing a negative count and anything above
// most. A most of 0 is a read that pages, which has no ceiling.
func addLimit(cmd *cobra.Command, n *int, most int, help string) {
	cmd.Flags().VarP(rowCount{n: n, most: most}, "limit", "n", help)
}

// addJSON binds --json to asJSON.
func addJSON(cmd *cobra.Command, asJSON *bool, what string) {
	cmd.Flags().BoolVar(asJSON, "json", false, "Write "+what+" to stdout as JSON")
}
