// Command clowder is the CLI of a clowder cluster (Linux, macOS, Windows).
package main

import (
	"fmt"
	"os"

	"github.com/webcodingcat/clowder/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowder:", err)
		os.Exit(1)
	}
}
