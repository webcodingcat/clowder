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
	// targetMounted: a stack on this host uses it, so it is primary here
	// and mounted.
	targetMounted
	// targetKept: a stack that left this host may still hold it (its
	// compose down has not worked yet); nothing changes until it stopped.
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

// copyFacts is what plan decides on: what the copy should be, what kind it
// is, and what the host has of it.
type copyFacts struct {
	target target
	// plain is a copy without DRBD (replicas 0), diskless a tiebreaker
	plain, diskless bool
	// initialized is the state's word (Volume.Initialized)
	initialized bool
	seen        storage.Observed
}

// Why plan refuses. Both need a human: guessing could lose data.
var (
	errDataGone    = errors.New("the volume holds data, but its only LV is gone; restore it from a backup")
	errOtherDevice = errors.New("another device is mounted at the copy's mount point; a human must look")
	// errTiebreakerMounted: a stack here names a volume of which this host
	// holds the tiebreaker; the placement never does that
	errTiebreakerMounted = errors.New("this host holds only a tiebreaker of the volume, which has no data to mount")
)

// plan returns the steps that bring a copy from what the host has to its
// target, in order; none when it is there. It only decides, so a pass that
// finds everything in place runs no tool.
//
// A volume that never held data is promoted with --force and formatted:
// its first copy has no UpToDate peer and no file system yet. Once it is
// initialized, never again: a copy then has the data or must get it from a
// peer. The state learns it from the first host that has the copy primary
// and mounted, which covers a crash between mkfs and the commit.
func plan(facts copyFacts) ([]step, error) {
	seen := facts.seen
	switch {
	case seen.OtherDevice:
		return nil, errOtherDevice
	case facts.plain && !seen.LV && facts.initialized:
		// a replicated copy that is missing gets its data from a peer; a
		// plain one has no peer
		return nil, errDataGone
	case facts.target == targetKept:
		return nil, nil
	case facts.diskless && facts.target == targetMounted:
		return nil, errTiebreakerMounted
	}
	var steps []step
	// a resource that is down or whose file changed needs ensure, which
	// runs drbdadm adjust
	resourceBehind := !facts.plain && (seen.Role == storage.RoleDown || !seen.ResourceFileCurrent)
	if !seen.LV && !facts.diskless || resourceBehind {
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
