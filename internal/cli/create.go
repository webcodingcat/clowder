package cli

import (
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/resource"
)

func newCreate(g *globals) *cobra.Command {
	create := &cobra.Command{Use: "create", Short: "Create a resource from the command line"}
	create.AddCommand(newCreateSecret(g))
	return create
}

func newCreateSecret(g *globals) *cobra.Command {
	var flags valueFlags
	command := &cobra.Command{
		Use:   "secret [<namespace>/]<name> (--from-literal <key>=<value> | --from-file <key>=<path|->)...",
		Short: "Create a Secret from values on the command line",
		Long: "Creates a Secret (in default if no namespace is given), as kubectl create secret generic\n" +
			"does, and refuses one that exists; change a Secret with apply. " + valueFlagsHelp + "\n" +
			"The values go to the cluster over the CLI's mTLS connection, and the cluster encrypts\n" +
			"them. Nothing is written to a file; for a Secret that goes into Git, use clowder secret\n" +
			"seal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			secret, err := secretFromFlags(args[0], flags, cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			// no question about the Hetzner token, as apply asks: a new
			// Secret cannot change it, because the configuration names
			// only Secrets that exist
			request := api.ApplyRequest{Resources: []resource.Object{secret}, Create: true}
			result, err := applyRequest(cmd.Context(), client, request, false)
			if err != nil {
				return err
			}
			if err := printWarnings(cmd.ErrOrStderr(), result); err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), result, false)
		},
	}
	flags.register(command)
	return command
}
