// Command ypl is the ypl command-line client.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/datapointchris/goclikit"

	"github.com/datapointchris/ypl/cli/internal/cli"
)

func main() {
	err := goclikit.Execute(context.Background(), cli.NewRootCommand(), cli.AutoUpdateConfig())
	if err == nil {
		return
	}
	if !errors.Is(err, goclikit.ErrReported) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	if errors.Is(err, goclikit.ErrUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}
