//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// applyAgainChangesNothing applies shop's directory again: every resource
// is unchanged, and the container runs on as it was.
func applyAgainChangesNothing(t *testing.T, server host) {
	app := server.service(t, "app")
	output := mustClowder(t, "apply", "-f", stackDir("shop"))
	if strings.Contains(output, ": applied") || !strings.Contains(output, "Stack default/shop: unchanged") {
		t.Errorf("the same files again:\n%s", output)
	}
	if again := server.service(t, "app"); again != app {
		t.Errorf("an unchanged apply recreated app: %s, then %s", app, again)
	}
}

// dryRunMakesNothing applies a new namespace with its stack as a dry run:
// the answer is a real apply's, and nothing is made.
func dryRunMakesNothing(t *testing.T, server host) {
	documents := documentIn("", resource.KindNamespace, "dry", "") +
		documentIn("dry", resource.KindStack, "web", "services: {app: {image: "+publicImage+"}}")
	output, err := clowderWithInput(strings.NewReader(documents), "apply", "--dry-run", "-f", "-")
	if err != nil || !strings.Contains(output, "Namespace dry: applied (dry run)") ||
		!strings.Contains(output, "Stack dry/web: applied (dry run)") {
		t.Fatalf("dry run: %v\n%s", err, output)
	}
	if _, err := clowder("get", "namespace", "dry"); err == nil {
		t.Error("the dry run made the namespace")
	}
	if containers := server.must(t, "docker ps -aq --filter name="+names.Project("dry", "web")); containers != "" {
		t.Errorf("the dry run started containers: %s", containers)
	}
}

// watchOutput is what clowder get --watch prints, read while it runs.
type watchOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *watchOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *watchOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// restartRecreatesAndTheWatchSeesIt restarts shop, unchanged since its
// apply, while clowder get stacks --watch runs: app gets a new container,
// and the watch shows the stack starting, then running.
func restartRecreatesAndTheWatchSeesIt(t *testing.T, server host) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var shown watchOutput
	watch := exec.CommandContext(ctx, clowderBinary, "get", "stacks", "--watch")
	watch.Dir = workDir
	watch.Env = cliEnvironment()
	watch.Stdout, watch.Stderr = &shown, &shown
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, agentPass, "the watch lists shop", func() error {
		if !strings.Contains(shown.String(), "shop") {
			return fmt.Errorf("output %q", shown.String())
		}
		return nil
	})

	app := server.service(t, "app")
	generation := stackInfo(t, "shop").generation
	mustClowder(t, "stack", "restart", "shop")
	waitRunning(t, "shop", generation)
	if again := server.service(t, "app"); again == "" || again == app {
		t.Errorf("the restart left app's container: %s, then %s", app, again)
	}
	eventually(t, agentPass, "the watch shows the restart", func() error {
		output := shown.String()
		starting := strings.Index(output, "starting")
		if starting < 0 || !strings.Contains(output[starting:], "running") {
			return fmt.Errorf("output:\n%s", output)
		}
		return nil
	})
	stop()
	_ = watch.Wait() // ended by the test
	t.Logf("clowder get stacks --watch:\n%s", shown.String())
}

// stackIn reads a stack of a namespace as clowder get lists it.
func stackIn(t *testing.T, namespace, name string) (api.StackStatus, error) {
	t.Helper()
	output, err := clowder("get", "stack", names.ID(namespace, name), "-o", "json")
	if err != nil {
		return api.StackStatus{}, err
	}
	var object resource.Object
	var status api.StackStatus
	if err := json.Unmarshal([]byte(output), &object); err != nil {
		return status, err
	}
	return status, json.Unmarshal(object.Status, &status)
}

// waitRunningIn waits until a stack of a namespace runs.
func waitRunningIn(t *testing.T, namespace, name string) {
	t.Helper()
	eventually(t, stackDeadline, "stack "+names.ID(namespace, name)+" running", func() error {
		status, err := stackIn(t, namespace, name)
		if err == nil && status.Status != "running" {
			err = fmt.Errorf("%s", status.Status)
		}
		return err
	})
}

// otherStack is a stack of the namespace other whose app mounts the volume
// kept at /data and runs command.
func otherStack(name, command string) string {
	return documentIn("other", resource.KindStack, name, `services:
  app:
    image: `+publicImage+`
    command: ["sh", "-c", "`+command+`"]
    stopGracePeriod: 1s
    volumes: [{volume: kept, path: /data}]
`)
}

// namespacesKeepTheirStacksApart runs a stack shop in the namespace other
// beside default's: on the host they share no project, container, network
// or LV. Its replicated volume then outlives it: the stack goes, and right
// after a new stack names the volume and finds the data. A second stack on
// that volume, a delete of it while a stack names it, and a delete without
// --with-data are refused. Without a stack the copy is unmounted and
// secondary, and the next stack finds the data again: the volume is
// promoted without --force and keeps its file system.
func namespacesKeepTheirStacksApart(t *testing.T, server host) {
	resources := filepath.Join(stackDir("other"), "other.yaml")
	if err := os.MkdirAll(stackDir("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resources, []byte(documentIn("", resource.KindNamespace, "other", "")+
		documentIn("other", resource.KindVolume, "kept", "class: bulk\nsize: 1G\nreplicas: 2")), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "apply", "-f", resources)
	apply := func(documents string) {
		t.Helper()
		if output, err := clowderWithInput(strings.NewReader(documents), "apply", "-f", "-"); err != nil {
			t.Fatalf("apply: %v\n%s", err, output)
		}
	}
	apply(otherStack("shop", "echo kept > /data/note && exec sleep infinity"))
	waitRunningIn(t, "other", "shop")

	containers := server.must(t, "docker ps --format '{{.Names}}'")
	networks := server.must(t, "docker network ls --format '{{.Name}}'")
	volumes := server.must(t, "lvs --noheadings -o lv_name")
	for listing, want := range map[string][]string{
		containers: {names.Container(names.Default, "shop", "app"), names.Container("other", "shop", "app")},
		networks:   {names.StackNetwork(names.Default, "shop"), names.StackNetwork("other", "shop")},
		volumes:    {volumeLV(t, "data"), names.VolumeName(names.ID("other", "kept"), 1)},
	} {
		for _, name := range want {
			if !strings.Contains(listing, name) {
				t.Errorf("no %s on the host:\n%s", name, listing)
			}
		}
	}

	// the stack goes, and the next takes its volume with the data
	mustClowder(t, "delete", "stack", "other/shop")
	apply(otherStack("reader", "exec sleep infinity"))
	waitRunningIn(t, "other", "reader")
	reader := names.Container("other", "reader", "app")
	if note := server.must(t, "docker exec "+reader+" cat /data/note"); note != "kept" {
		t.Errorf("the next stack found %q in the volume", note)
	}

	// what a stack uses stays
	if output, err := clowderWithInput(strings.NewReader(otherStack("thief", "exec sleep infinity")), "apply", "-f", "-"); err == nil ||
		!strings.Contains(output, "volume kept is stack other/reader's already") {
		t.Errorf("a second stack on the volume: %v\n%s", err, output)
	}
	if output, err := clowder("delete", "--with-data", "volume", "other/kept"); err == nil ||
		!strings.Contains(output, "is stack other/reader's") {
		t.Errorf("a delete of a used volume: %v\n%s", err, output)
	}
	if output, err := clowder("delete", "-f", resources); err == nil || !strings.Contains(output, "--with-data") {
		t.Errorf("a delete without --with-data: %v\n%s", err, output)
	}

	kept := names.VolumeName(names.ID("other", "kept"), 1)
	mustClowder(t, "delete", "stack", "other/reader")
	eventually(t, stackDeadline, "the copy without a stack unmounted and secondary", func() error {
		if output, err := server.run("findmnt --noheadings --mountpoint /var/lib/clowder/volumes/" + kept); err == nil {
			return fmt.Errorf("still mounted: %s", output)
		}
		if role := server.must(t, "drbdadm role "+kept); role != "Secondary" {
			return fmt.Errorf("its role is %s", role)
		}
		return nil
	})
	apply(otherStack("reader", "exec sleep infinity"))
	waitRunningIn(t, "other", "reader")
	if note := server.must(t, "docker exec "+reader+" cat /data/note"); note != "kept" {
		t.Errorf("the stack after the copy was secondary found %q in the volume", note)
	}

	mustClowder(t, "delete", "stack", "other/reader")
	mustClowder(t, "delete", "--with-data", "-f", resources)
	eventually(t, stackDeadline, "the volume's LV removed", func() error {
		if lvs := server.must(t, "lvs --noheadings -o lv_name"); strings.Contains(lvs, kept) {
			return fmt.Errorf("%s still there", kept)
		}
		return nil
	})
}
