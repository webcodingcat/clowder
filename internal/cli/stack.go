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
		Short: "The containers, logs, restarts and moves of a stack (apply, get and delete manage stacks)",
	}
	stack.AddCommand(newStackPs(g), newStackLogs(g), newStackRestart(g), newStackMove(g))
	return stack
}

// stackPath returns the API path for one stack's containers, logs or
// restart.
func stackPath(stack selection, what string) string {
	return "/v1/stacks/" + url.PathEscape(stack.namespace) + "/" + url.PathEscape(stack.name) + "/" + what
}

func newStackPs(g *globals) *cobra.Command {
	var selected selectFlags
	command := &cobra.Command{
		Use:   "ps [<namespace>/]<stack>",
		Short: "List the containers of a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stack, err := selected.resolve("stack", args[0])
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			var containers []api.ContainerInfo
			if err := client.Get(cmd.Context(), stackPath(stack, "ps"), &containers); err != nil {
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
			stack, err := selected.resolve("stack", args[0])
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			path := stackPath(stack, "logs")
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

func newStackRestart(g *globals) *cobra.Command {
	var selected selectFlags
	command := &cobra.Command{
		Use:   "restart [<namespace>/]<stack> [<service>...]",
		Short: "Recreate a stack's containers with their images pulled again",
		Long: "Recreates the containers of the named services, or of all services, and of every service\n" +
			"that depends on one of them, after pulling their images again. Use it for a new image\n" +
			"under the same tag, or for a retry. The stack's spec stays as it is.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stack, err := selected.resolve("stack", args[0])
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			request := api.RestartRequest{Services: args[1:]}
			if err := client.Act(cmd.Context(), stackPath(stack, "restart"), request, &api.Written{}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stack %s/%s: restarting\n", stack.namespace, stack.name)
			return err
		},
	}
	selected.register(command, false)
	return command
}

func newStackMove(g *globals) *cobra.Command {
	var selected selectFlags
	command := &cobra.Command{
		Use:   "move [<namespace>/]<stack> <host>",
		Short: "Move a stack to another host",
		Long: "Moves a stack to another host. Its volumes first get a copy there. Once the copies are\n" +
			"synced and the images are pulled, the stack stops on its old host and starts on the new\n" +
			"one. A volume of replicas 0 holds throw-away data, so it starts empty on the new host.\n" +
			"The move starts once the stack fits on the new host; get stacks says why it waits. A\n" +
			"move to the host that the stack runs on takes back an earlier request.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			stack, err := selected.resolve("stack", args[0])
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			request := api.MoveRequest{To: args[1]}
			if err := client.Act(cmd.Context(), stackPath(stack, "move"), request, &api.Written{}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Stack %s/%s: asked to move to %s; get stacks shows where it runs, or why it waits\n",
				stack.namespace, stack.name, args[1])
			return err
		},
	}
	selected.register(command, false)
	return command
}
