package parity

import "time"

// PendingFilesForFix is doc 02 §4's own "honest constraint" and doc 03
// §3.5 step 3 of the guided fix flow: before a fix reconstructs Disk from
// parity, what the change journal recorded changing on it since the last
// successful sync — exactly the files reconstruction cannot restore,
// because they were never part of the sync it reconstructs from. Files is
// named where the journal resolved a path, by entry name otherwise (Q13);
// Incomplete is true once the journal's own queue has overflowed since
// that sync, meaning Count and Files are a known-incomplete floor, not an
// exact count (Q13's own "incomplete, never folded into an exact-looking
// number").
type PendingFilesForFix struct {
	Disk       string
	LastSyncAt time.Time
	Count      int
	Files      []ChangedFile
	Incomplete bool
}

// FixPendingFiles builds diskID's own PendingFilesForFix from journal —
// the API call a guided fix's step 2/3 makes before offering to run at
// all (doc 03 §3.5): journal's own Files and Summary already track
// exactly "changed since the last successful sync" (ResetSince's own doc
// comment), so this is just naming that pair against the disk being
// fixed and the sync time the caller is displaying alongside it.
func FixPendingFiles(journal *Journal, diskID string, lastSyncAt time.Time) (PendingFilesForFix, error) {
	summary, err := journal.Summary(diskID)
	if err != nil {
		return PendingFilesForFix{}, err
	}
	files, err := journal.Files(diskID)
	if err != nil {
		return PendingFilesForFix{}, err
	}
	return PendingFilesForFix{
		Disk:       diskID,
		LastSyncAt: lastSyncAt,
		Count:      summary.Count,
		Files:      files,
		Incomplete: summary.Overflowed,
	}, nil
}
