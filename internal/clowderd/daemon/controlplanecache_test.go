package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// countedFetch answers with values and counts its calls; fail makes it
// fail.
type countedFetch struct {
	values map[string]string
	fail   bool
	calls  int
}

func (f *countedFetch) fetch(context.Context) (map[string]string, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("no leader")
	}
	return f.values, nil
}

func newTestCache(fetch *countedFetch) *controlPlaneCache[string] {
	return &controlPlaneCache[string]{
		what: "secret", fetch: fetch.fetch, version: func(value string) string { return value },
	}
}

func TestTheControlPlaneIsAskedOnlyForANewVersion(t *testing.T) {
	fetch := &countedFetch{values: map[string]string{"default/db": "v1"}}
	cache := newTestCache(fetch)
	ctx := context.Background()
	for range 3 {
		if value, err := cache.get(ctx, "default/db", "v1"); err != nil || value != "v1" {
			t.Fatalf("got %q, %v", value, err)
		}
	}
	if fetch.calls != 1 {
		t.Errorf("asked %d times for the same version, want once", fetch.calls)
	}

	// the state names v2, the control plane still has v1: try again later
	if _, err := cache.get(ctx, "default/db", "v2"); err == nil || !strings.Contains(err.Error(), "another version") {
		t.Errorf("a version the control plane does not have yet: %v", err)
	}
	fetch.values = map[string]string{"default/db": "v2"}
	if value, err := cache.get(ctx, "default/db", "v2"); err != nil || value != "v2" || fetch.calls != 3 {
		t.Errorf("got %q, %v after %d calls", value, err, fetch.calls)
	}

	// each answer replaces what is kept: what the host may no longer read goes
	fetch.values = map[string]string{"default/other": "v1"}
	if _, err := cache.get(ctx, "default/other", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(ctx, "default/db", "v2"); err == nil || !strings.Contains(err.Error(), "gives this host no secret default/db") {
		t.Errorf("a secret the control plane no longer gives: %v", err)
	}

	// forget drops what is kept, so the next get asks again
	calls := fetch.calls
	cache.forget()
	if _, err := cache.get(ctx, "default/other", "v1"); err != nil || fetch.calls != calls+1 {
		t.Errorf("after forget: %v after %d calls, want one more than %d", err, fetch.calls, calls)
	}
}

func TestAControlPlaneThatDoesNotAnswerIsAskedAgainOnlyAfterAPause(t *testing.T) {
	fetch := &countedFetch{fail: true}
	cache := newTestCache(fetch)
	ctx := context.Background()
	for range 3 {
		if _, err := cache.get(ctx, "default/db", "v1"); err == nil {
			t.Fatal("a value without an answer")
		}
	}
	if fetch.calls != 1 {
		t.Errorf("asked %d times within the pause, want once", fetch.calls)
	}
	fetch.fail, fetch.values = false, map[string]string{"default/db": "v1"}
	cache.failedAt = time.Now().Add(-askAgainAfter)
	if value, err := cache.get(ctx, "default/db", "v1"); err != nil || value != "v1" {
		t.Errorf("after the pause: %q, %v", value, err)
	}
}
