package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is what a running command prints, read while it runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForText waits up to 5 s for the output to hold text.
func waitForText(t *testing.T, out *syncBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q in:\n%s", text, out.String())
}

func TestGetWatchShowsTheListThenEachChange(t *testing.T) {
	cluster := startCluster(t)
	apply := func(document string) {
		t.Helper()
		if out, err := runWithInput(t, document, "--cluster", cluster, "apply", "-f", "-"); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
	}
	apply("apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n")

	ctx, stop := context.WithCancel(context.Background())
	var out syncBuffer
	ended := make(chan error, 1)
	go func() {
		root := NewRoot()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"--cluster", cluster, "get", "namespaces", "--watch"})
		ended <- root.ExecuteContext(ctx)
	}()
	waitForText(t, &out, "shop")
	apply("apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: blog}\n")
	waitForText(t, &out, "blog")
	if _, err := run(t, "--cluster", cluster, "delete", "namespace", "blog"); err != nil {
		t.Fatal(err)
	}
	waitForText(t, &out, "Namespace blog: deleted")
	stop()
	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("a watch stopped with Ctrl+C: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end")
	}
	if headers := strings.Count(out.String(), "NAME"); headers != 1 {
		t.Errorf("%d headers, want 1:\n%s", headers, out.String())
	}
}
