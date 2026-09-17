// Command ypl is the ypl command-line client.
package main

import (
	"os"

	"github.com/datapointchris/ypl/cli/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
