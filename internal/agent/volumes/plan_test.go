package volumes

import (
	"errors"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/storage"
)

func TestPlanBringsACopyFromWhatTheHostHasToItsTarget(t *testing.T) {
	// what the host has of a replicated copy
	var (
		missing      = storage.Observed{}
		afterBoot    = storage.Observed{LV: true, ResourceFileCurrent: true}
		secondary    = storage.Observed{LV: true, ResourceFileCurrent: true, Role: storage.RoleSecondary}
		primary      = storage.Observed{LV: true, ResourceFileCurrent: true, Role: storage.RolePrimary}
		mounted      = storage.Observed{LV: true, ResourceFileCurrent: true, Role: storage.RolePrimary, Mounted: true}
		newPeer      = storage.Observed{LV: true, Role: storage.RolePrimary, Mounted: true}
		otherDevice  = storage.Observed{LV: true, ResourceFileCurrent: true, Mounted: true, OtherDevice: true}
		plainThere   = storage.Observed{LV: true}
		plainMounted = storage.Observed{LV: true, Mounted: true}
	)
	tests := []struct {
		name    string
		facts   copyFacts
		want    []step
		wantErr error
	}{
		// a tiebreaker
		{
			name:  "a tiebreaker that still has the LV of its former disk",
			facts: copyFacts{target: targetSecondary, diskless: true, seen: secondary},
			want:  []step{stepEnsure},
		},
		{
			name:  "a tiebreaker in place",
			facts: copyFacts{target: targetSecondary, diskless: true, seen: storage.Observed{ResourceFileCurrent: true, Role: storage.RoleSecondary}},
		},

		// never initialized: the first copy becomes the volume's truth
		{
			name:  "missing, a stack here, never initialized",
			facts: copyFacts{target: targetMounted, seen: missing},
			want:  []step{stepEnsure, stepForcePromote, stepFormatAndMount, stepSetInitialized},
		},
		{
			name:  "missing, no stack here, never initialized",
			facts: copyFacts{target: targetSecondary, seen: missing},
			want:  []step{stepEnsure},
		},
		{
			name:  "secondary, a stack comes, never initialized",
			facts: copyFacts{target: targetMounted, seen: secondary},
			want:  []step{stepForcePromote, stepFormatAndMount, stepSetInitialized},
		},
		{
			name:  "mounted, the state does not know yet",
			facts: copyFacts{target: targetMounted, seen: mounted},
			want:  []step{stepSetInitialized},
		},
		{
			name:  "plain, missing, never initialized",
			facts: copyFacts{target: targetMounted, plain: true, seen: missing},
			want:  []step{stepEnsure, stepFormatAndMount, stepSetInitialized},
		},

		// initialized: never --force, never a new file system
		{
			name:  "missing, a stack here, initialized: a new replica",
			facts: copyFacts{target: targetMounted, initialized: true, seen: missing},
			want:  []step{stepEnsure, stepPromote, stepMount},
		},
		{
			name:  "missing, no stack here, initialized",
			facts: copyFacts{target: targetSecondary, initialized: true, seen: missing},
			want:  []step{stepEnsure},
		},
		{
			name:  "down after a boot, a stack here",
			facts: copyFacts{target: targetMounted, initialized: true, seen: afterBoot},
			want:  []step{stepEnsure, stepPromote, stepMount},
		},
		{
			name:  "down after a boot, no stack here",
			facts: copyFacts{target: targetSecondary, initialized: true, seen: afterBoot},
			want:  []step{stepEnsure},
		},
		{
			name:  "secondary, a stack here",
			facts: copyFacts{target: targetMounted, initialized: true, seen: secondary},
			want:  []step{stepPromote, stepMount},
		},
		{
			name:  "secondary, no stack here",
			facts: copyFacts{target: targetSecondary, initialized: true, seen: secondary},
		},
		{
			name:  "primary, unmounted, a stack here",
			facts: copyFacts{target: targetMounted, initialized: true, seen: primary},
			want:  []step{stepMount},
		},
		{
			name:  "primary, unmounted, no stack here",
			facts: copyFacts{target: targetSecondary, initialized: true, seen: primary},
			want:  []step{stepDemote},
		},
		{
			name:  "mounted, a stack here",
			facts: copyFacts{target: targetMounted, initialized: true, seen: mounted},
		},
		{
			name:  "mounted, no stack here",
			facts: copyFacts{target: targetSecondary, initialized: true, seen: mounted},
			want:  []step{stepUnmount, stepDemote},
		},
		{
			name:  "mounted, a new peer in the resource file",
			facts: copyFacts{target: targetMounted, initialized: true, seen: newPeer},
			want:  []step{stepEnsure},
		},
		{
			name:  "plain, mounted, a stack here",
			facts: copyFacts{target: targetMounted, plain: true, initialized: true, seen: plainMounted},
		},
		{
			name:  "plain, mounted, no stack here",
			facts: copyFacts{target: targetSecondary, plain: true, initialized: true, seen: plainMounted},
			want:  []step{stepUnmount},
		},
		{
			name:  "plain, unmounted, a stack here",
			facts: copyFacts{target: targetMounted, plain: true, initialized: true, seen: plainThere},
			want:  []step{stepMount},
		},
		{
			name:    "plain, missing, initialized: the data is gone",
			facts:   copyFacts{target: targetMounted, plain: true, initialized: true, seen: missing},
			wantErr: errDataGone,
		},

		// a stack that left may still hold it
		{
			name:  "mounted, held by a stack that left",
			facts: copyFacts{target: targetKept, initialized: true, seen: mounted},
		},
		{
			name:  "missing, held by a stack that left",
			facts: copyFacts{target: targetKept, initialized: true, seen: missing},
		},

		// a human must look
		{
			name:    "another device at the mount point",
			facts:   copyFacts{target: targetMounted, initialized: true, seen: otherDevice},
			wantErr: errOtherDevice,
		},
		{
			name:    "another device, no stack here",
			facts:   copyFacts{target: targetSecondary, initialized: true, seen: otherDevice},
			wantErr: errOtherDevice,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			steps, err := plan(test.facts)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error %v, want %v", err, test.wantErr)
			}
			if !slices.Equal(steps, test.want) {
				t.Errorf("steps %q, want %q", steps, test.want)
			}
		})
	}
}
