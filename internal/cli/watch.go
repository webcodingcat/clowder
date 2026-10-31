package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// watchObjects lists the selection, then shows each of its resources that
// changes (GET /v1/watch from the list's index) until ctx ends. When the
// agent cannot follow from that index any more (410, or the stream ends),
// it lists afresh and goes on, and says so on report.
func watchObjects(ctx context.Context, client *api.Client, out, report io.Writer, selected selection,
	output string,
) error {
	first := true
	for {
		if !first {
			_, _ = fmt.Fprintln(report, "clowder: the agent cannot follow from the list any more; listing afresh")
		}
		var list api.ResourceList
		if err := client.Get(ctx, selected.listPath(), &list); err != nil {
			return err
		}
		objects := slices.DeleteFunc(list.Items, func(object resource.Object) bool {
			return selected.name != "" && object.Metadata.Name != selected.name
		})
		if err := printWatched(out, selected, objects, output, first); err != nil {
			return err
		}
		first = false
		err := client.Watch(ctx, selected.watchPath(list.Index), func(event api.WatchEvent) error {
			return showEvent(ctx, client, out, selected, output, event)
		})
		var gone *api.StatusError
		switch {
		case ctx.Err() != nil:
			return nil // Ctrl+C
		case errors.Is(err, io.EOF), errors.As(err, &gone) && gone.Status == http.StatusGone:
			continue
		default:
			return err
		}
	}
}

// showEvent prints the resource of one event as it is now; a deleted one
// as "<Kind> <name>: deleted".
func showEvent(ctx context.Context, client *api.Client, out io.Writer, selected selection, output string,
	event api.WatchEvent,
) error {
	if selected.name != "" && event.Name != selected.name {
		return nil
	}
	if event.Deleted {
		id := event.Name
		if event.Namespace != "" {
			id = names.ID(event.Namespace, event.Name)
		}
		_, err := fmt.Fprintf(out, "%s %s: deleted\n", event.Kind, id)
		return err
	}
	changed := selected
	changed.namespace, changed.name = event.Namespace, event.Name
	var object resource.Object
	err := client.Get(ctx, changed.path(), &object)
	var missing *api.StatusError
	switch {
	case errors.As(err, &missing) && missing.Status == http.StatusNotFound:
		return nil // deleted since: its own event follows
	case err != nil:
		return err
	}
	return printWatched(out, selected, []resource.Object{object}, output, false)
}

// printWatched prints what a watch shows: the table's rows (with the header
// only before the first list), one JSON object each, or YAML documents
// after "---" (but the very first).
func printWatched(out io.Writer, selected selection, objects []resource.Object, output string, first bool) error {
	switch output {
	case "json":
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		for _, object := range objects {
			if err := encoder.Encode(object); err != nil {
				return err
			}
		}
		return nil
	case "yaml":
		if len(objects) > 0 && !first {
			if _, err := io.WriteString(out, "---\n"); err != nil {
				return err
			}
		}
		return printYAML(out, objects)
	}
	return printTable(out, selected.kind, objects, first)
}

// watchPath is the API's path of the selection's watch from an index.
func (s selection) watchPath(since uint64) string {
	query := url.Values{"kind": {strings.ToLower(s.kind)}, "since": {strconv.FormatUint(since, 10)}}
	switch {
	case !resource.Namespaced(s.kind):
	case s.all:
		query.Set("allNamespaces", "true")
	default:
		query.Set("namespace", s.namespace)
	}
	return "/v1/watch?" + query.Encode()
}
