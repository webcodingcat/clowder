package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
)

func newStack(g *globals) *cobra.Command {
	stack := &cobra.Command{Use: "stack", Short: "The Compose stacks of the cluster"}
	stack.AddCommand(newStackCreate(g), newStackApply(g), newStackDelete(g), newStackPs(g), newStackLogs(g))
	return stack
}

// The files of a stack's directory.
const (
	stackFile        = "clowder.yaml"
	stackSecretsFile = "secrets.sops.yaml"
)

func newStackCreate(g *globals) *cobra.Command {
	var dir, recipient string
	command := &cobra.Command{
		Use:   "create <name>",
		Short: "Register a stack and write the files of its directory",
		Long: "Registers the stack in the cluster and writes what is missing of its directory:\n" +
			stackFile + " (a Compose file), an empty " + stackSecretsFile + " and the .sops.yaml\n" +
			"that encrypts it for --age. Existing files are kept, so a stack directory from Git\n" +
			"is registered in a new cluster the same way, and a failed run can be repeated.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if dir == "" {
				dir = name
			}
			missing, err := missingStackFiles(name, dir, recipient)
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			if _, err := client.Post(cmd.Context(), "/v1/stacks/"+url.PathEscape(name), nil); err != nil {
				return err
			}
			if err := writeStackFiles(name, dir, recipient, missing); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s created in %s; clowder stack apply %s runs it\n",
				name, dir, name)
			return err
		},
	}
	command.Flags().StringVar(&dir, "dir", "", "the stack's directory (default: ./<name>)")
	command.Flags().StringVar(&recipient, "age", "",
		"your age key (age1... or an SSH public key) for "+stackSecretsFile)
	return command
}

// missingStackFiles checks a stack's directory before anything is
// registered: which files are missing, that an existing clowder.yaml is the
// stack's, and that --age is there if a SOPS file must be written.
func missingStackFiles(name, dir, recipient string) ([]string, error) {
	var missing []string
	for _, file := range []string{stackFile, stackSecretsFile, clusterdir.SOPSConfigFile} {
		if _, err := os.Stat(filepath.Join(dir, file)); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, file)
		} else if err != nil {
			return nil, err
		}
	}
	if !slices.Contains(missing, stackFile) {
		if err := compose.CheckNames(name, []string{filepath.Join(dir, stackFile)}); err != nil {
			return nil, err
		}
	}
	if recipient == "" && (slices.Contains(missing, stackSecretsFile) ||
		slices.Contains(missing, clusterdir.SOPSConfigFile)) {
		return nil, fmt.Errorf("--age is required to write %s and %s", stackSecretsFile, clusterdir.SOPSConfigFile)
	}
	return missing, nil
}

// writeStackFiles writes the missing files of a stack's directory.
func writeStackFiles(name, dir, recipient string, missing []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for _, file := range missing {
		path := filepath.Join(dir, file)
		var err error
		switch file {
		case stackFile:
			template := fmt.Sprintf("# The stack %s: a Compose file with clowder's rules (x-clowder blocks).\n"+
				"# clowder stack apply %s sends it to the cluster.\n"+
				"name: %s\n"+
				"x-clowder:\n"+
				"  # the stack's secrets, edited with sops; a service asks for them with\n"+
				"  # x-clowder.secrets (files in /run/secrets) or x-clowder.secret_env\n"+
				"  secrets_file: %s\n"+
				"services:\n  app:\n    image: nginx:stable\n", name, name, name, stackSecretsFile)
			err = os.WriteFile(path, []byte(template), 0o644)
		case stackSecretsFile:
			err = clusterdir.WriteSOPS(path, map[string]string{}, recipient)
		case clusterdir.SOPSConfigFile:
			err = clusterdir.WriteSOPSConfig(dir, `(^|/)secrets\.sops\.yaml$`, recipient)
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
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
			if len(files) == 0 {
				files = []string{filepath.Join(name, stackFile)}
			}
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
			secrets, err := readStackSecrets(stack, filepath.Dir(files[0]))
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			if _, err := client.Put(cmd.Context(), "/v1/stacks/"+url.PathEscape(name),
				api.StackApply{Source: string(source), Secrets: secrets}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s applied; clowder stack ps %s shows how it starts\n", name, name)
			return err
		},
	}
	command.Flags().StringSliceVarP(&files, "file", "f", nil,
		"the stack's files, repeatable; the first names the stack (default: <name>/"+stackFile+")")
	return command
}

// readStackSecrets decrypts the stack's secrets file with the user's age key
// and takes the values the stack asks for: each a string, not empty. Whoever
// cannot decrypt the file cannot apply a stack with secrets. No message
// names a value.
func readStackSecrets(stack *compose.Stack, dir string) (map[string]string, error) {
	asked := stack.SecretNames()
	if len(asked) == 0 {
		return nil, nil
	}
	file := stack.SecretsFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	cleartext, err := clusterdir.ReadSOPS(file)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(cleartext, &document); err != nil || len(document.Content) == 0 ||
		document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is not a map of names to secrets", file)
	}
	inFile := map[string]*yaml.Node{}
	entries := document.Content[0].Content
	for index := 0; index+1 < len(entries); index += 2 { // key, value, key, value, ...
		inFile[entries[index].Value] = entries[index+1]
	}
	values := map[string]string{}
	for _, name := range asked {
		value, ok := inFile[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s has no secret %s", file, name)
		case value.Kind != yaml.ScalarNode || value.Tag != "!!str":
			return nil, fmt.Errorf("secret %s in %s is not a string; quote it", name, file)
		case value.Value == "":
			return nil, fmt.Errorf("secret %s in %s is empty", name, file)
		}
		values[name] = value.Value
	}
	return values, nil
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
	_, _ = fmt.Fprintln(table, "NAME\tSTATUS\tHOST\tPRIORITY\tGENERATION\tSTOPPED\tCREATED BY") // tabwriter reports errors on Flush
	for _, stack := range stacks {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", stack.Name, stack.Status, stack.Host,
			stack.Priority, stack.Generation, strings.Join(stack.Stopped, ","), stack.CreatedBy)
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
