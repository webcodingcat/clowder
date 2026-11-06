package volumes

import (
	"context"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
)

const trimInterval = 7 * 24 * time.Hour

// Trim gives the free blocks of this host's mounted copies back to their
// pools every trimInterval. The time of the last trim is kept in a file, so
// that restarts of the agent do not delay it. Trim runs next to the
// reconciler's loop and shares only the fields that mu guards.
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

// trimNow records the time only once every mounted copy is trimmed; with
// none (also before the first pass), it tries again in the next round.
//
// fstrim is the one write to a volume outside a job (DEVELOPING.md,
// Volumes). It runs only on a copy whose DRBD has quorum, because without
// quorum fstrim hangs in state D, and it claims the volume so that no job
// unmounts it meanwhile.
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
