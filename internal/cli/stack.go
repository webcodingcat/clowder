package cli

import (
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newStack(g *globals) *cobra.Command {
	stack := &cobra.Command{
		Use:   "stack",
		Short: "The containers and logs of a stack (apply, get and delete manage stacks)",
	}
	stack.AddCommand(newStackPs(g), newStackLogs(g))
	return stack
}

// stackPath is the API's path of one stack's containers or logs.
func stackPath(selected selectFlags, name, what string) (string, error) {
	stack, err := selected.resolve("stack", name)
	if err != nil {
		return "", err
	}
	return "/v1/stacks/" + url.PathEscape(stack.namespace) + "/" + url.PathEscape(stack.name) + "/" + what, nil
}

func newStackPs(g *globals) *cobra.Command {
	var selected selectFlags
	command := &cobra.Command{
		Use:   "ps [<namespace>/]<stack>",
		Short: "List the containers of a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := stackPath(selected, args[0], "ps")
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			var containers []api.ContainerInfo
			if err := client.Get(cmd.Context(), path, &containers); err != nil {
				return err
			}
			return printContainers(cmd.OutOrStdout(), containers)
		},
	}
	selected.register(command, false)
	return command
}

func printContainers(out io.Writer, containers []api.ContainerInfo) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "SERVICE\tSTATE\tHEALTH\tSTATUS") // tabwriter reports errors on Flush
	for _, container := range containers {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", container.Service, container.State, container.Health, container.Status)
	}
	return table.Flush()
}

func newStackLogs(g *globals) *cobra.Command {
	var selected selectFlags
	var follow bool
	command := &cobra.Command{
		Use:   "logs [<namespace>/]<stack>",
		Short: "Show the logs of a stack's containers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := stackPath(selected, args[0], "logs")
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			if follow {
				path += "?follow=true"
			}
			return client.Stream(cmd.Context(), path, cmd.OutOrStdout())
		},
	}
	selected.register(command, false)
	command.Flags().BoolVarP(&follow, "follow", "f", false, "keep following the logs")
	return command
}
