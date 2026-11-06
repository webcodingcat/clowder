// Package check is clowderd check: it checks that a host is set up for
// clowder and changes nothing.
package check

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/hostcheck"
)

// errCheckFailed makes clowderd check exit with status 1 after printing the
// results; the results say what is wrong.
var errCheckFailed = errors.New("the host is not ready for clowder")

// NewCommand returns clowderd check.
func NewCommand() *cobra.Command {
	var pools []string
	var floatingIP string
	command := &cobra.Command{
		Use:   "check",
		Short: "Check that this host is set up for clowder (changes nothing)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options := hostcheck.Options{Pools: pools}
			if floatingIP != "" {
				address, err := netip.ParseAddr(floatingIP)
				if err != nil {
					return fmt.Errorf("--floating-ip: %w", err)
				}
				if !address.Is4() {
					return fmt.Errorf("--floating-ip %s: clowder uses IPv4 Floating IPs", address)
				}
				options.FloatingIP = address
			}
			results := hostcheck.Run(cmd.Context(), hostcheck.System{}, options)
			if err := hostcheck.Print(cmd.OutOrStdout(), results); err != nil {
				return err
			}
			if hostcheck.Failed(results) {
				return errCheckFailed
			}
			return nil
		},
	}
	command.Flags().StringSliceVar(&pools, "pool", nil, "thin pool of a storage class (vg/thinpool), repeatable")
	command.Flags().StringVar(&floatingIP, "floating-ip", "", "the cluster's Floating IP")
	return command
}
