package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

func newApplyResources(g *globals) *cobra.Command {
	var from string
	var dryRun, yes bool
	command := &cobra.Command{
		Use:   "apply -f <file|directory|->",
		Short: "Create or change resources: the configuration, namespaces, volumes, secrets, stacks",
		Long: "Applies the resources of a YAML file, of the YAML files of a directory, or of stdin (-), in\n" +
			"the order in which they appear: each file from top to bottom, and a directory's files in\n" +
			"order of their names. The cluster checks each resource against the resources before it,\n" +
			"so put a Secret before what names it, and a namespace before what it holds. The cluster\n" +
			"applies all resources or none. A Secret's values are encrypted in spec.data and tagged\n" +
			"!secret (clowder secret encrypt writes the Secret). Applying a resource that has not\n" +
			"changed changes nothing. If an apply changes the Hetzner token (the ClusterConfiguration,\n" +
			"or the Secret it names), it first shows what the new token sees and asks for\n" +
			"confirmation; --yes applies it without asking.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return usage("name the resources with -f <file|directory|->")
			}
			objects, err := readResourceFiles(from, cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			request := api.ApplyRequest{Resources: objects}
			if !dryRun && !yes && mayChangeToken(objects) {
				if err := confirmToken(cmd, client, request); err != nil {
					return err
				}
			}
			result, err := applyRequest(cmd.Context(), client, request, dryRun)
			if err != nil {
				return err
			}
			if err := printWarnings(cmd.ErrOrStderr(), result); err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), result, dryRun)
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "a file, a directory of files, or - for stdin")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "check everything against the cluster, change nothing")
	command.Flags().BoolVar(&yes, "yes", false, "apply a new Hetzner token without asking")
	return command
}

// applyRequest sends an apply, and says what to do if it is too large.
func applyRequest(ctx context.Context, client *api.Client, request api.ApplyRequest, dryRun bool) (api.ChangeResult, error) {
	result, err := client.ApplyResources(ctx, request, dryRun)
	var statusError *api.StatusError
	if errors.As(err, &statusError) && statusError.Status == http.StatusRequestEntityTooLarge {
		return result, fmt.Errorf("%w; apply the directories of the namespaces one at a time", err)
	}
	return result, err
}

// mayChangeToken tells whether an apply may change the Hetzner token,
// because it holds the configuration or a Secret of names.System. Only then
// does the CLI ask the cluster first.
func mayChangeToken(objects []resource.Object) bool {
	return slices.ContainsFunc(objects, func(object resource.Object) bool {
		return object.Kind == resource.KindClusterConfiguration ||
			object.Kind == resource.KindSecret && object.Metadata.Namespace == names.System
	})
}

// confirmToken first sends the apply as a dry run. If the apply changes the
// Hetzner token, it shows what the token sees and asks for confirmation,
// as host remove does. It never refuses because of what the token sees: a
// token may be changed on purpose before its project is. Without a
// terminal, --yes is the only way to confirm.
func confirmToken(cmd *cobra.Command, client *api.Client, request api.ApplyRequest) error {
	tried, err := applyRequest(cmd.Context(), client, request, true)
	if err != nil {
		return err
	}
	if tried.HetznerToken == nil || !applies(tried) {
		return nil // the real apply answers the same refusal
	}
	if err := printTokenCheck(cmd.ErrOrStderr(), *tried.HetznerToken); err != nil {
		return err
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return errors.New("the apply changes the Hetzner token: confirm it with --yes")
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), "Apply the new Hetzner token? Type yes to go on: "); err != nil {
		return err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) { // at EOF the line is what was typed
		return fmt.Errorf("read the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("the new Hetzner token was not confirmed; nothing was applied")
	}
	return nil
}

// applies tells whether an answer accepted every resource.
func applies(result api.ChangeResult) bool {
	return !slices.ContainsFunc(result.Results, func(each api.ResourceResult) bool {
		return each.Outcome != api.Applied && each.Outcome != api.Deleted && each.Outcome != api.Unchanged
	})
}

// printTokenCheck says what a new Hetzner token sees.
func printTokenCheck(out io.Writer, check api.TokenCheck) error {
	var found string
	switch {
	case check.Error != "":
		found = "could not be checked: " + check.Error
	case check.Seen == 0:
		found = fmt.Sprintf("sees none of the servers of the %d hosts: fencing and the Floating IP would stop", check.Hosts)
	default:
		found = fmt.Sprintf("sees the servers of %d of %d hosts", check.Seen, check.Hosts)
	}
	_, err := fmt.Fprintf(out, "The new Hetzner token %s.\n", found)
	return err
}

// printWarnings prints what the cluster warns about, and in a dry run what
// a new Hetzner token sees, to stderr.
func printWarnings(out io.Writer, result api.ChangeResult) error {
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	if result.HetznerToken != nil {
		return printTokenCheck(out, *result.HetznerToken)
	}
	return nil
}

// printResults prints what became of each resource. A request that did
// not go through makes the command fail.
func printResults(out io.Writer, result api.ChangeResult, dryRun bool) error {
	suffix := ""
	if dryRun {
		suffix = " (dry run)"
	}
	for _, each := range result.Results {
		ref := resource.Ref{Kind: each.Kind, Namespace: each.Namespace, Name: each.Name}
		line := ref.String() + ": " + each.Outcome + suffix
		if each.Message != "" {
			line += ": " + each.Message
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if !applies(result) {
		return fmt.Errorf("nothing of the %d resources went through", len(result.Results))
	}
	return nil
}

// errDiffers is diff's answer when an apply would change something. The
// command ends with exit code 1, as kubectl diff does, without a message.
var errDiffers = errors.New("the resources differ from the cluster's")

func newDiff(g *globals) *cobra.Command {
	var from string
	command := &cobra.Command{
		Use:   "diff -f <file|directory|->",
		Short: "Show what an apply of the resources would change",
		Long: "Asks the cluster to try the resources as apply would, without applying them, and shows\n" +
			"what would change: the resources that are new, and the fields of the other resources'\n" +
			"specs that differ (for a Secret, its keys, never its values; a value that was only\n" +
			"encrypted again is not a change). A resource that the cluster would refuse is shown with\n" +
			"the reason. Like kubectl diff, it ends with exit code 0 when nothing would change, 1 when\n" +
			"something would change, and above 1 on an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return usage("name the resources with -f <file|directory|->")
			}
			err := diffResources(cmd, g, from)
			if err != nil && !errors.Is(err, errDiffers) {
				return diffFailed{err}
			}
			return err
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "a file, a directory of files, or - for stdin")
	return command
}

// diffResources reads the resources and has the cluster try them.
func diffResources(cmd *cobra.Command, g *globals, from string) error {
	objects, err := readResourceFiles(from, cmd.InOrStdin(), cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	client, err := g.connect()
	if err != nil {
		return err
	}
	result, err := applyRequest(cmd.Context(), client, api.ApplyRequest{Resources: objects}, true)
	if err != nil {
		return err
	}
	if err := printWarnings(cmd.ErrOrStderr(), result); err != nil {
		return err
	}
	return printDiff(cmd.OutOrStdout(), result)
}

// printDiff prints what each resource would change. It returns errDiffers
// if any resource would change, and an error if the cluster would refuse
// one.
func printDiff(out io.Writer, result api.ChangeResult) error {
	differs := false
	for _, each := range result.Results {
		ref := resource.Ref{Kind: each.Kind, Namespace: each.Namespace, Name: each.Name}
		var what string
		switch {
		case each.Outcome == api.Unchanged:
			continue
		case each.Outcome == api.Applied && each.Created:
			what = "new"
		case each.Outcome == api.Applied:
			what = "differs in " + strings.Join(each.Differs, ", ")
		case each.Outcome == api.NotApplied:
			continue
		default:
			what = each.Outcome
			if each.Message != "" {
				what += ": " + each.Message
			}
		}
		differs = true
		if _, err := fmt.Fprintf(out, "%s: %s\n", ref, what); err != nil {
			return err
		}
	}
	switch {
	case !applies(result):
		return errors.New("the cluster would refuse the apply")
	case differs:
		return errDiffers
	}
	_, err := fmt.Fprintln(out, "nothing would change")
	return err
}
