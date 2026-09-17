// Command ypl is the client for a ypl server.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/datapointchris/goclikit"

	"github.com/datapointchris/ypl/cli/cmd"
)

func main() {
	err := goclikit.Execute(context.Background(), cmd.NewRootCommand(), cmd.AutoUpdateConfig())
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
