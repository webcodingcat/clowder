//go:build linux

// Command clowderd is the agent that runs on every clowder host (Linux only).
package main

import (
	"fmt"
	"os"

	"github.com/webcodingcat/clowder/internal/agent"
)

func main() {
	if err := agent.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		os.Exit(1)
	}
}
