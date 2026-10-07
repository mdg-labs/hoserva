package main

import (
	"errors"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// wireMigrationChecklist is what main.go calls, after wireMigration, to make
// the post-migration checklist reachable (doc 05 §4 steps 18 and 21 to 25): the
// migrator reads the job history, the notification channels and the nightly
// chain's schedule (api.ChecklistSources) and asks the array store when the
// migration finished (the stamp FinishMigration writes), and keeps the acknowledgements in its
// session row. It fails when wireMigration, the notification service or the
// schedule service has not run, so a missing piece is an error instead of
// operations that answer 501.
func wireMigrationChecklist(handler *api.Handler, jobs *job.Store, arrays *store.ArrayStore) error {
	if handler.Migration == nil {
		return errors.New("the migration session is not wired")
	}
	if handler.Notify == nil || handler.Schedules == nil || jobs == nil || arrays == nil {
		return errors.New("the notification service, the schedule service, the job store or the array store is not wired")
	}
	handler.Migration.ChecklistRecords = api.ChecklistSources(jobs, handler.Notify, handler.Schedules, arrays)
	return nil
}
