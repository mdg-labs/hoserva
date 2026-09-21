package cache

// DefaultThresholdPercent is doc 09 §2's own default mover trigger: run
// when the cache is at least this full, alongside the scheduled nightly
// run and any manual run.
const DefaultThresholdPercent = 70.0

// ThresholdExceeded reports whether a cache holding usedBytes of
// totalBytes has crossed thresholdPercent (doc 09 §2 "Triggers"; doc 02
// §3). totalBytes <= 0 never exceeds — a cache this call cannot measure
// is not evidence that it is full. The poll loop that measures cache
// usage and calls this on a schedule is job/scheduler wiring outside this
// package (see doc.go).
func ThresholdExceeded(usedBytes, totalBytes int64, thresholdPercent float64) bool {
	if totalBytes <= 0 {
		return false
	}
	return float64(usedBytes)/float64(totalBytes)*100 >= thresholdPercent
}
