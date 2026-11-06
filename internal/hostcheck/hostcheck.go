// Package hostcheck checks the operator base of a host: what the operator
// sets up (with cloud-init, Ansible or by hand) and clowderd relies on. It
// only reads and never changes the host. clowderd check runs it.
package hostcheck

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// Status is the outcome of one check.
type Status int

const (
	// OK means the host is as clowder needs it.
	OK Status = iota
	// Warn means clowder works, but something is off.
	Warn
	// Fail means clowder cannot run correctly on this host.
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// Result is the outcome of one check with what was found.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Host is what the checks read. The real one (System) reads files and runs
// commands. Tests use a fake.
type Host interface {
	ReadFile(path string) ([]byte, error)
	// Run runs a command and returns its standard output.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// Addresses returns the IPv4 addresses on the host's interfaces.
	Addresses() ([]netip.Addr, error)
}

// Options are what the operator tells the check about this cluster.
type Options struct {
	// Pools are the thin pools ("vg/thinpool") of the storage classes.
	Pools []string
	// FloatingIP must be on an interface of every host.
	FloatingIP netip.Addr
}

// check is one named check.
type check struct {
	name string
	run  func(ctx context.Context, host Host, options Options) (Status, string)
}

// Run runs every check and returns the results in a fixed order.
func Run(ctx context.Context, host Host, options Options) []Result {
	var results []Result
	for _, next := range checks {
		status, detail := next.run(ctx, host, options)
		results = append(results, Result{Name: next.name, Status: status, Detail: detail})
	}
	return results
}

// Failed tells whether any result failed.
func Failed(results []Result) bool {
	for _, result := range results {
		if result.Status == Fail {
			return true
		}
	}
	return false
}

// Print writes the results as a table for people.
func Print(out io.Writer, results []Result) error {
	for _, result := range results {
		line := fmt.Sprintf("%-4s %s", result.Status, result.Name)
		if result.Detail != "" {
			line += ": " + result.Detail
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

// readTrimmed reads a file and trims white space.
func readTrimmed(host Host, path string) (string, error) {
	content, err := host.ReadFile(path)
	return strings.TrimSpace(string(content)), err
}
