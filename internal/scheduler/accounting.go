package scheduler

// Fits tells whether a new volume fits a thin pool without overcommit. The
// sizes of the pool's volumes plus the new one must stay within the pool
// minus its reserve. The reserve is a percentage of the pool, kept free
// for snapshots and metadata. A thin pool that runs full stops every
// volume in it, so clowder never promises more space than there is.
func Fits(poolBytes int64, reservePercent int, allocated []int64, sizeBytes int64) bool {
	usable := poolBytes - poolBytes*int64(reservePercent)/100
	total := sizeBytes
	for _, size := range allocated {
		total += size
	}
	return sizeBytes > 0 && total <= usable
}
