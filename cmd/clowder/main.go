// Command clowder is the CLI of a clowder cluster (Linux, macOS, Windows).
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/logging"
)

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.Text, slog.LevelInfo))
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowder:", err)
		os.Exit(cli.ExitCode(err))
	}
}
