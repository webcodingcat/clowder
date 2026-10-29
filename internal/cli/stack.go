package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/compose"
)

func newStack(g *globals) *cobra.Command {
	stack := &cobra.Command{Use: "stack", Short: "The Compose stacks of the cluster"}
	stack.AddCommand(newStackApply(g), newStackDelete(g), newStackPs(g), newStackLogs(g))
	return stack
}

func newStackApply(g *globals) *cobra.Command {
	var files []string
	command := &cobra.Command{
		Use:   "apply <name>",
		Short: "Check a Compose stack here and run it on the cluster",
		Long: "Reads the Compose files (variables from the environment, env files included), checks\n" +
			"what clowder cannot run and sends the stack as one self-contained file.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			stack, err := compose.Load(cmd.Context(), compose.LoadOptions{
				Name: name, WorkingDir: filepath.Dir(files[0]), Files: files, Environment: environment(),
			})
			if err != nil {
				return err
			}
			source, err := stack.Source()
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			if _, err := client.Put(cmd.Context(), "/v1/stacks/"+url.PathEscape(name),
				api.StackApply{Source: string(source)}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s applied; clowder stack ps %s shows how it starts\n", name, name)
			return err
		},
	}
	command.Flags().StringSliceVarP(&files, "file", "f", []string{"compose.yaml"}, "Compose file, repeatable")
	return command
}

// environment is the process environment for ${VARIABLE} in Compose files.
func environment() map[string]string {
	variables := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			variables[name] = value
		}
	}
	return variables
}

func newStackDelete(g *globals) *cobra.Command {
	var deleteVolumes bool
	command := &cobra.Command{
		Use:   "delete <name>",
		Short: "Stop and remove a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			path := "/v1/stacks/" + url.PathEscape(args[0])
			if deleteVolumes {
				path += "?volumes=delete"
			}
			if _, err := client.Delete(cmd.Context(), path); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", args[0])
			return err
		},
	}
	command.Flags().BoolVar(&deleteVolumes, "delete-volumes", false,
		"delete the stack's volumes with all their data, too (needed when it has volumes)")
	return command
}

func newStackPs(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "ps [<name>]",
		Short: "List the stacks, or the containers of one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				var stacks []api.StackInfo
				if err := client.Get(cmd.Context(), "/v1/stacks", &stacks); err != nil {
					return err
				}
				return printStacks(out, stacks)
			}
			var containers []api.ContainerInfo
			if err := client.Get(cmd.Context(), "/v1/stacks/"+url.PathEscape(args[0])+"/ps", &containers); err != nil {
				return err
			}
			return printContainers(out, containers)
		},
	}
}

func printStacks(out io.Writer, stacks []api.StackInfo) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tSTATUS\tHOST\tPRIORITY\tGENERATION\tSTOPPED") // tabwriter reports errors on Flush
	for _, stack := range stacks {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\t%s\n", stack.Name, stack.Status, stack.Host,
			stack.Priority, stack.Generation, strings.Join(stack.Stopped, ","))
	}
	return table.Flush()
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
	var follow bool
	command := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show the logs of a stack's containers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			path := "/v1/stacks/" + url.PathEscape(args[0]) + "/logs"
			if follow {
				path += "?follow=1"
			}
			return client.Stream(cmd.Context(), path, cmd.OutOrStdout())
		},
	}
	command.Flags().BoolVarP(&follow, "follow", "f", false, "keep following the logs")
	return command
}
