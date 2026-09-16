package job

import "time"

// Job is one persisted long-running operation (doc 01 §4). It is this
// package's own domain type — internal/api maps it to api/gen/go's
// generated Job schema; nothing in this package depends on generated API
// types.
type Job struct {
	ID           string
	Type         Type
	Class        Class
	Status       Status
	Progress     *int
	Resumable    bool
	Cancellable  bool
	ResourceIDs  []string
	Checkpoint   []byte
	ErrorCode    string
	ErrorMessage string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// ListFilter narrows ListJobs (api/openapi.yaml's listJobs operation).
type ListFilter struct {
	Class  *Class
	Status *Status
	Limit  int
}
