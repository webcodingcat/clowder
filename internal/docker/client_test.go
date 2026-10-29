package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeDocker serves the bits of Docker's API the client uses on a Unix
// socket.
func fakeDocker(t *testing.T) (string, *[]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket")
	}
	path := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "events "+r.URL.Query().Get("filters"))
		_, _ = fmt.Fprintln(w, `{"Action":"die","Actor":{"ID":"c1","Attributes":{"exitCode":"137","clowder.stack":"shop"}}}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "start "+r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /containers/{id}/restart", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `[{"Id":"c1","State":"exited","Labels":{"clowder.stack":"shop"}}]`)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return path, &calls
}

func TestTheClientSpeaksDockersAPI(t *testing.T) {
	path, calls := fakeDocker(t)
	client := NewClient(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, _ := client.Events(ctx, "clowder.stack")
	select {
	case event := <-events:
		if event.Action != "die" || event.ContainerID != "c1" || event.ExitCode != 137 || event.Labels["clowder.stack"] != "shop" {
			t.Errorf("event %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	if err := client.Start(ctx, "c1"); err != nil {
		t.Error(err)
	}
	if err := client.Restart(ctx, "gone"); err == nil {
		t.Error("a failed restart went unnoticed")
	}
	containers, err := client.Containers(ctx, "clowder.stack")
	if err != nil || len(containers) != 1 || containers[0].State != "exited" {
		t.Errorf("containers %+v, %v", containers, err)
	}
	if len(*calls) < 2 || (*calls)[1] != "start c1" {
		t.Errorf("calls %v", *calls)
	}
}
