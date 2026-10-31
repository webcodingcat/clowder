package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
)

// nextEvent waits up to 5 s for the watch's next event.
func nextEvent(t *testing.T, events <-chan WatchEvent) WatchEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return WatchEvent{}
	}
}

func TestAWatchStreamsTheChangesAfterItsList(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var list ResourceList
	if err := client.Get(ctx, "/v1/resources/namespace", &list); err != nil {
		t.Fatal(err)
	}
	events := make(chan WatchEvent, 10)
	watching, stop := context.WithCancel(ctx)
	ended := make(chan error, 1)
	go func() {
		ended <- client.Watch(watching, fmt.Sprintf("/v1/watch?kind=namespace&since=%d", list.Index),
			func(event WatchEvent) error {
				events <- event
				return nil
			})
	}()
	// a secret is another kind: the watch leaves it out
	for _, resources := range [][]resource.Object{
		{object(resource.KindSecret, "default", "token", `{"value": "s3cret"}`)},
		{object(resource.KindNamespace, "", "shop", "")},
	} {
		if _, err := client.ApplyResources(ctx, ApplyRequest{Resources: resources}, false); err != nil {
			t.Fatal(err)
		}
	}
	if event := nextEvent(t, events); event.Kind != resource.KindNamespace || event.Name != "shop" || event.Deleted ||
		event.ResourceVersion <= list.Index {
		t.Errorf("applied: %+v", event)
	}
	if _, err := client.DeleteResources(ctx, DeleteRequest{Resources: []resource.Ref{
		{Kind: resource.KindNamespace, Name: "shop"},
	}}, false); err != nil {
		t.Fatal(err)
	}
	if event := nextEvent(t, events); event.Name != "shop" || !event.Deleted {
		t.Errorf("deleted: %+v", event)
	}
	stop()
	if err := <-ended; err != nil {
		t.Errorf("a watch the caller stopped: %v", err)
	}
	// the agent's stop ends a watch, which would hold up its Shutdown
	go func() {
		ended <- client.Watch(ctx, fmt.Sprintf("/v1/watch?kind=namespace&since=%d", list.Index),
			func(WatchEvent) error { return nil })
	}()
	time.Sleep(100 * time.Millisecond) // the watch is under way
	cluster.apis[0].EndStreams()
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Errorf("a watch the agent ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("EndStreams left the watch running")
	}
}

func TestAWatchFromTooFarBackOrAheadIsRefused(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	ctx := context.Background()
	if _, err := client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", ""),
	}}, false); err != nil {
		t.Fatal(err)
	}
	leader := cluster.clusters[0]
	leader.mu.Lock()
	index := leader.state.Index
	leader.changes.Reset(index) // as after a snapshot install
	leader.mu.Unlock()
	watchWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { watchWaitTimeout = 30 * time.Second })
	for path, want := range map[string]int{
		fmt.Sprintf("/v1/watch?kind=namespace&since=%d", index-1):   http.StatusGone,
		fmt.Sprintf("/v1/watch?kind=namespace&since=%d", index+100): http.StatusGatewayTimeout,
		"/v1/watch?kind=namespace":                                  http.StatusBadRequest,
		"/v1/watch?kind=secret&since=0":                             http.StatusBadRequest, // no namespace named
		"/v1/watch?kind=pod&since=0":                                http.StatusBadRequest,
	} {
		err := client.Watch(ctx, path, func(WatchEvent) error { return nil })
		if statusOf(err) != want {
			t.Errorf("%s: want HTTP %d, got %v", path, want, err)
		}
	}
}
