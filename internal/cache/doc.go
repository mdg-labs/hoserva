// Package cache implements the mover (doc 09 §2, #53): relocating a
// cache-then-move share's files from cache to the array once they are no
// longer being written to, so the cache stays fast and the array holds
// the long-term data. It never picks which array disk a file lands on —
// it writes through the share's own array-only mergerfs mount
// (pool.MoverTargetPath), so mergerfs places every file exactly as it
// would have placed a direct write (CLAUDE.md: "one placement
// algorithm").
//
// Run is the algorithm itself, independent of the job system, so it can
// be exercised at L1 with plain temp directories and at L2 against a real
// mergerfs mount in the loop-device lab without any of internal/job's
// machinery. The RunFunc that registers TypeMover with the scheduler
// (job.RunFunc, doc 01 §4) is a thin adapter over Run's RunHooks and
// belongs with the rest of that wiring in internal/job, alongside the
// other job types' own *_run.go files — outside this package's own
// scope.
package cache
