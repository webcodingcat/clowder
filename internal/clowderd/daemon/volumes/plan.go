package volumes

import (
	"errors"

	"github.com/webcodingcat/clowder/internal/storage"
)

// target is what a copy on this host should be.
type target int

const (
	// targetSecondary: the copy exists, unmounted and secondary, so a stack
	// on another host can take the volume.
	targetSecondary target = iota
	// targetMounted: a stack on this host uses the copy, so it is primary
	// here and mounted.
	targetMounted
	// targetKept: a stack that left this host may still hold the copy (its
	// compose down has not succeeded yet). Nothing changes until the stack
	// has stopped.
	targetKept
)

func (t target) String() string {
	switch t {
	case targetSecondary:
		return "secondary"
	case targetMounted:
		return "mounted"
	case targetKept:
		return "kept"
	default:
		return "unknown"
	}
}

// step is one thing to do to a copy, as the storage driver does it.
type step string

const (
	stepEnsure         step = "ensure"
	stepPromote        step = "promote"
	stepForcePromote   step = "promote --force"
	stepMount          step = "mount"
	stepFormatAndMount step = "mount --format"
	stepSetInitialized step = "set initialized"
	stepUnmount        step = "unmount"
	stepDemote         step = "demote"
)

type copyFacts struct {
	target target
	// plain is a copy without DRBD (replicas 0), diskless a tiebreaker
	plain, diskless bool
	// initialized is what the state says (Volume.Initialized)
	initialized bool
	seen        storage.Observed
}

// The reasons why plan refuses. Both need a human, because a guess could
// lose data.
var (
	errDataGone    = errors.New("the volume holds data, but its only LV is gone; restore it from a backup")
	errOtherDevice = errors.New("another device is mounted at the copy's mount point; a human must look")
	// errTiebreakerMounted: a local stack names a volume of which this host
	// holds only the tiebreaker; the placement never does that
	errTiebreakerMounted = errors.New("this host holds only a tiebreaker of the volume, which has no data to mount")
)

// plan only decides, so a pass that finds everything in place runs no tool.
//
// A volume that never held data is promoted with --force and formatted,
// because its first copy has no UpToDate peer and no file system yet. After
// the volume is initialized, this never happens again: a copy then has the
// data or must get it from a peer. The state learns that the volume is
// initialized from the first host that has the copy primary and mounted,
// which covers a crash between mkfs and the commit.
func plan(facts copyFacts) ([]step, error) {
	seen := facts.seen
	switch {
	case seen.OtherDevice:
		return nil, errOtherDevice
	case facts.plain && !seen.LV && facts.initialized:
		// a missing replicated copy gets its data from a peer, but a
		// plain copy has no peer
		return nil, errDataGone
	case facts.target == targetKept:
		return nil, nil
	case facts.diskless && facts.target == targetMounted:
		return nil, errTiebreakerMounted
	}
	var steps []step
	// a resource that is down or whose file changed needs ensure, which
	// runs drbdadm adjust. A tiebreaker that still has the LV of its former
	// disk needs ensure too, which removes the LV.
	resourceBehind := !facts.plain && (seen.Role == storage.RoleDown || !seen.ResourceFileCurrent)
	missingLV := !seen.LV && !facts.diskless
	leftoverLV := seen.LV && facts.diskless
	if missingLV || leftoverLV || resourceBehind {
		steps = append(steps, stepEnsure)
	}
	primary := seen.Role == storage.RolePrimary
	switch facts.target {
	case targetMounted:
		switch {
		case !facts.plain && !primary && facts.initialized:
			steps = append(steps, stepPromote)
		case !facts.plain && !primary:
			steps = append(steps, stepForcePromote)
		}
		switch {
		case !seen.Mounted && facts.initialized:
			steps = append(steps, stepMount)
		case !seen.Mounted:
			steps = append(steps, stepFormatAndMount)
		}
		if !facts.initialized {
			steps = append(steps, stepSetInitialized)
		}
	case targetSecondary:
		if seen.Mounted {
			steps = append(steps, stepUnmount)
		}
		if !facts.plain && primary {
			steps = append(steps, stepDemote)
		}
	}
	return steps, nil
}
