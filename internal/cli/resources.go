package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/units"
)

func newApplyResources(g *globals) *cobra.Command {
	var from string
	var dryRun bool
	command := &cobra.Command{
		Use:   "apply -f <file|directory|->",
		Short: "Create or change resources: namespaces, volumes, secrets, stacks",
		Long: "Applies the resources of a YAML file, of the YAML files of a directory, or of stdin (-),\n" +
			"namespaces first, stacks last. A secret may come in a SOPS file of its own (*.sops.yaml), which is\n" +
			"decrypted with your age key. A resource applied as it is changes nothing. A resource\n" +
			"the cluster refuses stops the apply: those after it are not tried, as they usually need it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return usage("name the resources with -f <file|directory|->")
			}
			objects, err := readResources(from, cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slices.SortStableFunc(objects, func(a, b resource.Object) int { return cmp.Compare(applyRank(a.Kind), applyRank(b.Kind)) })
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			result, err := client.ApplyResources(cmd.Context(), api.ApplyRequest{Resources: objects}, dryRun)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), result, dryRun)
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "a file, a directory of files, or - for stdin")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "check everything against the cluster, change nothing")
	return command
}

// applyRank orders an apply: a namespace before what it holds, a stack
// after the volumes and secrets it names. A delete goes the other way
// round.
func applyRank(kind string) int {
	switch kind {
	case resource.KindNamespace:
		return 0
	case resource.KindStack:
		return 2
	default:
		return 1
	}
}

func newGet(g *globals) *cobra.Command {
	var selected selectFlags
	var output string
	command := &cobra.Command{
		Use:   "get <kind> [[<namespace>/]<name>]",
		Short: "List resources of a kind, or show one",
		Long: "Kinds: " + kindWords() + ", each singular or plural. The namespace comes with -n or in the\n" +
			"name (shop/data), else it is default; -A lists every namespace.\n" +
			"-o yaml shows what apply takes again (a secret shows its digest, never its value).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "yaml" && output != "json" {
				return usage("--output %q: table, yaml or json", output)
			}
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			selection, err := selected.resolve(args[0], name)
			if err != nil {
				return err
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			var objects []resource.Object
			if selection.name == "" {
				var list api.ResourceList
				if err := client.Get(cmd.Context(), selection.listPath(), &list); err != nil {
					return err
				}
				objects = list.Items
			} else {
				var object resource.Object
				if err := client.Get(cmd.Context(), selection.path(), &object); err != nil {
					return err
				}
				objects = []resource.Object{object}
			}
			return printObjects(cmd.OutOrStdout(), selection, objects, output)
		},
	}
	selected.register(command, true)
	command.Flags().StringVarP(&output, "output", "o", "table", "table, yaml (applies again) or json")
	return command
}

func newDelete(g *globals) *cobra.Command {
	var selected selectFlags
	var from string
	var dryRun bool
	command := &cobra.Command{
		Use:   "delete (<kind> [<namespace>/]<name> | -f <file|directory|->)",
		Short: "Delete resources that nothing uses",
		Long: "Deletes one resource, or those of a file, a directory or stdin (-) in the reverse order\n" +
			"of an apply. A namespace goes only when empty, a volume with all its data.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var refs []resource.Ref
			switch {
			case from != "" && len(args) > 0:
				return usage("name the resources either with -f or as <kind> <name>, not both")
			case from != "" && selected.namespace != "":
				return usage("-f takes the namespaces from the files; drop -n")
			case from != "":
				objects, err := readResources(from, cmd.InOrStdin(), cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				// the reverse of an apply: b before a
				slices.SortStableFunc(objects, func(a, b resource.Object) int { return cmp.Compare(applyRank(b.Kind), applyRank(a.Kind)) })
				for _, object := range objects {
					refs = append(refs, object.Ref())
				}
			case len(args) != 2:
				return usage("name a resource: delete <kind> [<namespace>/]<name>, or -f <file|directory|->")
			default:
				selection, err := selected.resolve(args[0], args[1])
				if err != nil {
					return err
				}
				refs = []resource.Ref{{Kind: selection.kind, Namespace: selection.namespace, Name: selection.name}}
			}
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			result, err := client.DeleteResources(cmd.Context(), api.DeleteRequest{Resources: refs}, dryRun)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), result, dryRun)
		},
	}
	selected.register(command, false)
	command.Flags().StringVarP(&from, "filename", "f", "", "delete the resources of a file, a directory, or - for stdin")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "check everything against the cluster, change nothing")
	return command
}

// printResults prints what became of each resource; anything but applied,
// deleted or unchanged makes the command fail.
func printResults(out io.Writer, result api.ChangeResult, dryRun bool) error {
	suffix := ""
	if dryRun {
		suffix = " (dry run)"
	}
	failed := 0
	for _, each := range result.Results {
		ref := resource.Ref{Kind: each.Kind, Namespace: each.Namespace, Name: each.Name}
		line := ref.String() + ": " + each.Outcome + suffix
		if each.Message != "" {
			line += ": " + each.Message
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
		if each.Outcome != api.Applied && each.Outcome != api.Deleted && each.Outcome != api.Unchanged {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d resources did not go through", failed, len(result.Results))
	}
	return nil
}

// selectFlags are -n and -A, as kubectl has them.
type selectFlags struct {
	namespace string
	all       bool
}

func (f *selectFlags) register(command *cobra.Command, lists bool) {
	command.Flags().StringVarP(&f.namespace, "namespace", "n", "", "the namespace (default: default)")
	if lists {
		command.Flags().BoolVarP(&f.all, "all-namespaces", "A", false, "list the resources of every namespace")
	}
}

// selection is what a command names: a kind, a namespace (none for a
// namespace or every namespace), and a name (none for a list).
type selection struct {
	kind      string
	namespace string
	name      string
	all       bool
}

// resolve reads a kind and a name as users type them, with -n and -A: the
// namespace comes with -n or in the name (shop/data), else it is default.
// Where both name it, they must agree.
func (f selectFlags) resolve(kindWord, name string) (selection, error) {
	kind, err := kindOf(kindWord)
	if err != nil {
		return selection{}, err
	}
	chosen := selection{kind: kind, name: name, all: f.all}
	namespace, inName, hasNamespace := strings.Cut(name, "/")
	switch {
	case f.namespace != "" && f.all:
		return selection{}, usage("-n names one namespace, -A all; not both")
	case hasNamespace && (namespace == "" || inName == ""):
		return selection{}, usage("%q: write <namespace>/<name>, both parts", name)
	case !resource.Namespaced(kind) && (f.namespace != "" || f.all || hasNamespace):
		return selection{}, usage("a %s lives in no namespace: drop -n, -A and the namespace in the name", kind)
	case !resource.Namespaced(kind):
		return chosen, nil
	case f.all && name != "":
		return selection{}, usage("-A lists every namespace; it takes no name")
	case hasNamespace && f.namespace != "" && f.namespace != namespace:
		return selection{}, usage("%s names namespace %s, but -n %s", name, namespace, f.namespace)
	case hasNamespace:
		chosen.namespace, chosen.name = namespace, inName
	case f.namespace != "":
		chosen.namespace = f.namespace
	case !f.all:
		chosen.namespace = names.Default
	}
	return chosen, nil
}

// kindOf reads a kind as users type it: in lower case, singular or plural
// (volume, volumes).
func kindOf(word string) (string, error) {
	if kind, ok := resource.KindOf(word); ok {
		return kind, nil
	}
	if kind, ok := resource.KindOf(strings.TrimSuffix(word, "s")); ok {
		return kind, nil
	}
	return "", usage("no kind of resource %q; there are %s", word, kindWords())
}

func kindWords() string {
	var words []string
	for _, kind := range resource.Kinds {
		words = append(words, strings.ToLower(kind))
	}
	return strings.Join(words, ", ")
}

// listPath is the API's path of the selection's list.
func (s selection) listPath() string {
	path := "/v1/resources/" + strings.ToLower(s.kind)
	switch {
	case !resource.Namespaced(s.kind):
		return path
	case s.all:
		return path + "?allNamespaces=true"
	default:
		return path + "?namespace=" + url.QueryEscape(s.namespace)
	}
}

// path is the API's path of the one resource selected.
func (s selection) path() string {
	if !resource.Namespaced(s.kind) {
		return "/v1/resources/namespace/" + url.PathEscape(s.name)
	}
	return "/v1/resources/" + strings.ToLower(s.kind) + "/" + url.PathEscape(s.namespace) + "/" + url.PathEscape(s.name)
}

// printObjects prints resources as a table, as YAML documents (which apply
// takes again) or as JSON: one object, or a list of them.
func printObjects(out io.Writer, selected selection, objects []resource.Object, output string) error {
	switch output {
	case "json":
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if selected.name != "" {
			return encoder.Encode(objects[0])
		}
		return encoder.Encode(objects)
	case "yaml":
		for index, object := range objects {
			document, err := yamlOf(object)
			if err != nil {
				return err
			}
			if index > 0 {
				document = append([]byte("---\n"), document...)
			}
			if _, err := out.Write(document); err != nil {
				return err
			}
		}
		return nil
	}
	return printTable(out, selected.kind, objects)
}

// yamlOf writes a resource as YAML, its fields in the order the API wrote
// them.
func yamlOf(object resource.Object) ([]byte, error) {
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	// JSON is YAML: read as a node tree, it keeps the order of the fields
	var document yaml.Node
	if err := yaml.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	blockStyle(&document)
	return yaml.Marshal(&document)
}

// blockStyle drops the styles JSON gave the nodes ({...}, "..."), so YAML
// writes them as people do and quotes only where needed.
func blockStyle(node *yaml.Node) {
	node.Style = 0
	for _, child := range node.Content {
		blockStyle(child)
	}
}

// digestShown is how much of a secret's digest a table shows: enough to see
// a change.
const digestShown = 12

func printTable(out io.Writer, kind string, objects []resource.Object) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	// tabwriter reports errors on Flush
	switch kind {
	case resource.KindNamespace:
		_, _ = fmt.Fprintln(table, "NAME\tOWNER\tGENERATION")
		for _, object := range objects {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%d\n", object.Metadata.Name, object.Metadata.Owner, object.Metadata.Generation)
		}
	case resource.KindVolume:
		_, _ = fmt.Fprintln(table, "NAMESPACE\tNAME\tCLASS\tSIZE\tREPLICAS\tSTATUS\tCOPIES")
		for _, object := range objects {
			var spec resource.VolumeSpec
			var status api.VolumeStatus
			if err := decodeParts(object, &spec, &status); err != nil {
				return err
			}
			var copies []string
			for _, copied := range status.Copies {
				copies = append(copies, copied.Host)
			}
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", object.Metadata.Namespace, object.Metadata.Name,
				spec.Class, units.FormatSize(int64(spec.Size)), spec.Replicas, status.Status, strings.Join(copies, ","))
		}
	case resource.KindSecret:
		_, _ = fmt.Fprintln(table, "NAMESPACE\tNAME\tGENERATION\tDIGEST")
		for _, object := range objects {
			var status api.SecretStatus
			if err := decodeParts(object, nil, &status); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(table, "%s\t%s\t%d\t%s\n", object.Metadata.Namespace, object.Metadata.Name,
				object.Metadata.Generation, status.Digest[:min(len(status.Digest), digestShown)])
		}
	case resource.KindStack:
		// what the stack waits for last: it is long, and empty for most
		// stacks; a running stack waits too when a volume it names finds no
		// place
		_, _ = fmt.Fprintln(table, "NAMESPACE\tNAME\tSTATUS\tHOST\tPRIORITY\tGENERATION\tPUSHED OUT\tWAITING FOR")
		for _, object := range objects {
			var status api.StackStatus
			if err := decodeParts(object, nil, &status); err != nil {
				return err
			}
			priority := strconv.Itoa(status.Priority)
			if status.Default {
				priority += " (default)"
			}
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", object.Metadata.Namespace, object.Metadata.Name,
				status.Status, status.Host, priority, object.Metadata.Generation, strings.Join(status.PushedOut, ","),
				status.WaitReason)
		}
	}
	return table.Flush()
}

// decodeParts reads a resource's spec and status as the API wrote them;
// nil skips one.
func decodeParts(object resource.Object, spec, status any) error {
	for _, part := range []struct {
		raw    json.RawMessage
		target any
	}{{object.Spec, spec}, {object.Status, status}} {
		if part.target == nil || len(part.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(part.raw, part.target); err != nil {
			return fmt.Errorf("read %s: %w", object.Ref(), err)
		}
	}
	return nil
}
