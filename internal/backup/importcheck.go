package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"github.com/mdg-labs/hoserva/internal/store"
)

// The refusal codes CheckImport reports. They are the error codes
// importConfig answers with, so the preview's blockers and the import's
// refusals cannot disagree.
const (
	RefusalIncompatibleArchive = "incompatible_archive"
	RefusalOtherInstallation   = "archive_other_installation"
	RefusalArrayMismatch       = "archive_array_mismatch"
	RefusalUnsafeRestorePath   = "restore_path_unsafe"
)

// ImportRefusal is one reason an archive may not be restored in place.
type ImportRefusal struct {
	Code    string
	Message string
}

// ImportCheck is what CheckImport found: both schema versions, and the
// refusal that stops an in-place import, nil when nothing does.
type ImportCheck struct {
	ArchiveSchemaVersion string
	LiveSchemaVersion    string
	Refusal              *ImportRefusal
}

// UnreadableArchiveError is returned by CheckImport when the archive's
// database has no readable schema version: the archive is not usable at
// all, which importConfig reports as invalid_archive.
type UnreadableArchiveError struct{ Err error }

func (e *UnreadableArchiveError) Error() string {
	return fmt.Sprintf("reading archive schema version: %v", e.Err)
}

func (e *UnreadableArchiveError) Unwrap() error { return e.Err }

// CheckImport decides whether the database at archiveDB, the staged
// state.db of a verified archive, may replace live's content in place: the
// same schema version, then CheckRestorable's installation and array
// checks. It is the one check both importConfig and previewConfigImport
// run. A database that cannot be read is an error and never a pass; neither
// database is written.
func CheckImport(ctx context.Context, live *sql.DB, archiveDB string) (ImportCheck, error) {
	arc, err := openArchiveDB(archiveDB)
	if err != nil {
		return ImportCheck{}, err
	}
	defer func() { _ = arc.Close() }()
	return checkImport(ctx, live, arc)
}

func openArchiveDB(archiveDB string) (*sql.DB, error) {
	arc, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: archiveDB, RawQuery: "mode=ro"}).String())
	if err != nil {
		return nil, fmt.Errorf("opening the archive's database: %w", err)
	}
	return arc, nil
}

func checkImport(ctx context.Context, live, arc *sql.DB) (ImportCheck, error) {
	var check ImportCheck
	var err error
	check.ArchiveSchemaVersion, err = (&store.Runner{DB: arc}).CurrentVersion(ctx)
	if err != nil {
		return ImportCheck{}, &UnreadableArchiveError{Err: err}
	}
	check.LiveSchemaVersion, err = (&store.Runner{DB: live}).CurrentVersion(ctx)
	if err != nil {
		return ImportCheck{}, fmt.Errorf("reading live database schema version: %w", err)
	}
	if check.ArchiveSchemaVersion != check.LiveSchemaVersion {
		check.Refusal = &ImportRefusal{
			Code:    RefusalIncompatibleArchive,
			Message: fmt.Sprintf("archive schema version %s does not match the running database's %s", check.ArchiveSchemaVersion, check.LiveSchemaVersion),
		}
		return check, nil
	}

	err = checkRestorable(ctx, live, arc)
	var mismatch *ArrayMismatchError
	switch {
	case err == nil:
	case errors.Is(err, ErrArchiveOtherInstallation):
		check.Refusal = &ImportRefusal{Code: RefusalOtherInstallation, Message: err.Error()}
	case errors.As(err, &mismatch):
		check.Refusal = &ImportRefusal{Code: RefusalArrayMismatch, Message: err.Error()}
	default:
		return ImportCheck{}, fmt.Errorf("comparing the archive with this installation: %w", err)
	}
	return check, nil
}
