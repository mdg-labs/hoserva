package parity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// FileMeta is the owner, group and mode one tracked file or directory had
// when the last parity-changing sync finished (#626). Disk is the data
// disk's mount point and RelPath the path under it, the keys share_usage
// uses. Mode holds the permission bits and setuid, setgid and sticky.
type FileMeta struct {
	Disk, RelPath string
	Dir           bool
	UID, GID      uint32
	Mode          uint32
}

// fileMetaBatch is how many rows one write transaction inserts or deletes.
// A write transaction holds the central database's only write lock, so none
// may grow with the array's file count: at a few microseconds a row, a batch
// holds it for tens of milliseconds, far below the busy timeout other
// writers wait.
const fileMetaBatch = 2000

// FileMetaStore persists file_metadata in the central SQLite database (D4)
// through the sqlc-generated internal/store/db package: Replace is its
// only write, run by a sync that changed parity, and Get its only read,
// run by a fix for each file it restored. It is never read on a request.
type FileMetaStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewFileMetaStore wraps db for file-metadata persistence.
func NewFileMetaStore(db *sql.DB) *FileMetaStore {
	return &FileMetaStore{db: db, q: storedb.New(db)}
}

// FileMeta returns the file-metadata store over the same database s wraps.
// SnapraidEngine reaches the store through the share-usage store the daemon
// already gives it: both are computed from tracked state at the end of a
// sync, so there is one thing to wire.
func (s *UsageStore) FileMeta() *FileMetaStore {
	return NewFileMetaStore(s.db)
}

// Replace writes every row produce emits as a new generation of the record
// and makes it the current one. produce streams its rows through emit
// rather than returning them, because an array of millions of files must
// not be held in memory, and it runs outside any database transaction:
// emit commits the rows in batches of fileMetaBatch under a generation no
// reader looks at yet, and only once produce has returned does one
// single-row statement switch the record to the new generation. A failed
// produce, a failed write or a cancelled ctx therefore leaves the previous
// record exactly as it was, and the longest write transaction Replace
// holds is one batch of fileMetaBatch rows. Rows of generations that are
// not current (the previous record afterwards, a failed attempt's rows) are
// deleted in batches of the same size, the next Replace retrying any a
// cleanup could not.
func (s *FileMetaStore) Replace(ctx context.Context, produce func(emit func(FileMeta) error) error) error {
	current, err := s.currentGeneration(ctx)
	if err != nil {
		return err
	}
	if err := s.deleteOtherGenerations(ctx, current); err != nil {
		return err
	}
	highest, err := s.q.MaxFileMetadataGeneration(ctx)
	if err != nil {
		return fmt.Errorf("parity: reading file metadata generation: %w", err)
	}
	next := max(current, highest) + 1

	batch := make([]FileMeta, 0, fileMetaBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.insertBatch(ctx, next, batch)
		batch = batch[:0]
		return err
	}
	emit := func(m FileMeta) error {
		batch = append(batch, m)
		if len(batch) < fileMetaBatch {
			return nil
		}
		return flush()
	}
	err = produce(emit)
	if err == nil {
		err = flush()
	}
	if err == nil {
		if err = s.q.SetFileMetadataGeneration(ctx, next); err != nil {
			err = fmt.Errorf("parity: switching file metadata to the new generation: %w", err)
		}
	}
	if err != nil {
		// Best effort: the rows of a generation nothing reads are only
		// space, and the next Replace deletes them if this cannot.
		_ = s.deleteOtherGenerations(context.WithoutCancel(ctx), current)
		return err
	}
	_ = s.deleteOtherGenerations(context.WithoutCancel(ctx), next)
	return nil
}

func (s *FileMetaStore) currentGeneration(ctx context.Context) (int64, error) {
	g, err := s.q.GetFileMetadataGeneration(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("parity: reading file metadata generation: %w", err)
	}
	return g, nil
}

func (s *FileMetaStore) insertBatch(ctx context.Context, generation int64, rows []FileMeta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("parity: beginning file metadata transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	for _, m := range rows {
		var dir int64
		if m.Dir {
			dir = 1
		}
		if err := q.InsertFileMetadata(ctx, storedb.InsertFileMetadataParams{
			Generation:     generation,
			DiskMountpoint: m.Disk,
			RelPath:        m.RelPath,
			IsDir:          dir,
			Uid:            int64(m.UID),
			Gid:            int64(m.GID),
			Mode:           int64(m.Mode),
		}); err != nil {
			return fmt.Errorf("parity: inserting file metadata for %s: %w", m.RelPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("parity: committing file metadata: %w", err)
	}
	return nil
}

// deleteOtherGenerations deletes every row not of generation keep, one
// batch (one statement, its own transaction) at a time.
func (s *FileMetaStore) deleteOtherGenerations(ctx context.Context, keep int64) error {
	for {
		n, err := s.q.DeleteFileMetadataOtherGenerations(ctx, storedb.DeleteFileMetadataOtherGenerationsParams{Generation: keep, Limit: fileMetaBatch})
		if err != nil {
			return fmt.Errorf("parity: deleting superseded file metadata: %w", err)
		}
		if n < fileMetaBatch {
			return nil
		}
	}
}

// Recorded reports whether any sync has recorded file metadata yet.
func (s *FileMetaStore) Recorded(ctx context.Context) (bool, error) {
	_, err := s.q.GetFileMetadataGeneration(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("parity: reading file metadata: %w", err)
	}
	return true, nil
}

// Get returns what was recorded for relPath on disk, with ok false when
// nothing was: the path was never tracked, or no sync has recorded it yet.
func (s *FileMetaStore) Get(ctx context.Context, disk, relPath string) (FileMeta, bool, error) {
	row, err := s.q.GetFileMetadata(ctx, storedb.GetFileMetadataParams{DiskMountpoint: disk, RelPath: relPath})
	if errors.Is(err, sql.ErrNoRows) {
		return FileMeta{}, false, nil
	}
	if err != nil {
		return FileMeta{}, false, fmt.Errorf("parity: reading file metadata for %s: %w", relPath, err)
	}
	return FileMeta{Disk: disk, RelPath: relPath, Dir: row.IsDir == 1, UID: uint32(row.Uid), GID: uint32(row.Gid), Mode: uint32(row.Mode)}, true, nil
}
