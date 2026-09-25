package update

import (
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/job"
)

var (
	ErrChecksumMismatch = errors.New("update: package checksum does not match the signed SHA256SUMS")
	ErrNotAvailable     = errors.New("update: no newer release on the configured channel")
	ErrNoPrevious       = errors.New("update: no previous version to roll back to")
	ErrConfirmRequired  = errors.New("update: this operation requires confirm=true")
	ErrIndexURL         = errors.New("update: refusing to fetch a URL that is not the configured release index or a named release asset")
	ErrUnknownChannel   = errors.New("update: unrecognized channel")
)

// ErrBlocked is an update/rollback refused because a storage-class job
// is running. Message names that job.
type ErrBlocked struct {
	Job *job.Job
}

func (e ErrBlocked) Error() string {
	if e.Job == nil {
		return "update: refused while a storage job is running"
	}
	return fmt.Sprintf("update: refused while %s job %s (%s) is running", e.Job.Class, e.Job.ID, e.Job.Type)
}
