package volumes

import (
	"context"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
)

// trimInterval is how often the volumes give free blocks back.
const trimInterval = 7 * 24 * time.Hour

// Trim gives the free blocks of this host's mounted copies back to their
// pools once every trimInterval, until ctx ends. The time of the last trim
// is a file, so restarts of the agent do not put it off. It runs beside the
// reconciler's loop and shares only what mu guards.
func (r *Reconciler) Trim(ctx context.Context) {
	for {
		info, err := os.Stat(r.files.LastTrim)
		if err != nil || time.Since(info.ModTime()) >= trimInterval {
			r.trimNow(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// trimNow trims the copies the last pass saw mounted, and remembers when
// once each was trimmed. With none (before the first pass too) it tries
// again in the next round, and so it does while one was skipped or failed.
//
// fstrim writes to the volume (it discards blocks), so it is the one write
// the agent runs outside a job: a trim takes long and must not hold up the
// copy's steps meanwhile. Only a copy whose DRBD has its quorum is trimmed,
// a plain one always: without quorum fstrim would hang in state D until the
// quorum is back, the timeout of internal/command lets the agent go on,
// and the next copy waits. Each copy's volume is claimed while fstrim runs,
// so no job unmounts it meanwhile; a busy one is skipped, and the driver
// skips one that is no longer mounted.
func (r *Reconciler) trimNow(ctx context.Context) {
	r.mu.Lock()
	mounted := slices.Clone(r.mounted)
	seen := maps.Clone(r.seen)
	r.mu.Unlock()
	if len(mounted) == 0 {
		return
	}
	trimmed := 0
	for _, volume := range mounted {
		if copyNow := seen[volume.ID]; volume.Resource != nil && !copyNow.Quorum {
			r.logger.Info("trim later: the volume's copy has no quorum", "volume", volume.ID)
			continue
		}
		if !r.claim(volume.ID) {
			continue
		}
		err := r.storage.Trim(ctx, volume)
		r.release(ctx, volume.ID, false)
		if err != nil {
			r.logger.Error("trim a volume", "volume", volume.ID, "error", err)
			continue
		}
		trimmed++
	}
	if trimmed < len(mounted) {
		return // the next round, in an hour, tries again
	}
	if err := atomicfile.Write(r.files.LastTrim, nil, 0o600); err != nil {
		r.logger.Error("remember the trim", "error", err)
	}
}
