// Package storage keeps the volumes of this host: thin LVs in LVM thin
// pools, DRBD 9 resources for replicated volumes, ext4 and the mount under
// /var/lib/clowder/volumes. Every command that touches a volume runs with a
// timeout and is left behind if it hangs (internal/command): without quorum,
// DRBD suspends I/O.
package storage

// Fits tells whether a new volume fits a thin pool without overcommit: the
// sizes of all its volumes and the new one stay within the pool less its
// reserve (percent of the pool, kept for snapshots and metadata). Thin pools
// that run full stop every volume in them, so clowder never promises more
// than there is.
func Fits(poolBytes int64, reservePercent int, allocated []int64, sizeBytes int64) bool {
	usable := poolBytes - poolBytes*int64(reservePercent)/100
	total := sizeBytes
	for _, size := range allocated {
		total += size
	}
	return sizeBytes > 0 && total <= usable
}
