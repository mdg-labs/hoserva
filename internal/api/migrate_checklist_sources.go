package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// checklistJobPage is how many succeeded jobs of one type the checklist reads at
// a time while it looks for the first one that matches.
const checklistJobPage = 200

// ChecklistSources are the records the post-migration checklist reads in the
// daemon: the job history for the relocation, the initial sync, the scrub and
// the fix, the array record's stamp of when the migration finished
// (store.ArrayStore.MigrationFinishedAt), the notification channels, and the
// nightly chain's schedule.
func ChecklistSources(jobs *job.Store, channels *notify.Service, schedules *ScheduleService, finished func(context.Context) (time.Time, bool, error)) migrate.ChecklistSources {
	return migrate.ChecklistSources{
		Jobs:     checklistJobs(jobs),
		Finished: finished,
		Channels: checklistChannels(channels),
		Schedule: checklistSchedule(schedules),
	}
}

// checklistJobs reads the succeeded jobs of one type, oldest first, a page at a
// time. A job whose parameters cannot be read is skipped and logged: it cannot
// show the checklist what it did, so it never counts as having done it.
func checklistJobs(jobs *job.Store) func(context.Context, string, func(migrate.JobRecord) bool) error {
	return func(ctx context.Context, jobType string, visit func(migrate.JobRecord) bool) error {
		for offset := 0; ; offset += checklistJobPage {
			page, err := jobs.ListSucceededOfType(ctx, job.Type(jobType), checklistJobPage, offset)
			if err != nil {
				return err
			}
			for _, j := range page {
				rec, err := checklistJobRecord(j)
				if err != nil {
					log.Printf("the migration checklist skips job %s: %v", j.ID, err)
					continue
				}
				if visit(rec) {
					return nil
				}
			}
			if len(page) < checklistJobPage {
				return nil
			}
		}
	}
}

func checklistJobRecord(j *job.Job) (migrate.JobRecord, error) {
	rec := migrate.JobRecord{ID: j.ID, CreatedAt: j.CreatedAt}
	if j.StartedAt != nil {
		rec.StartedAt = *j.StartedAt
	}
	if j.FinishedAt != nil {
		rec.FinishedAt = *j.FinishedAt
	}
	switch j.Type {
	case job.TypeSync:
		opts, err := job.SyncOptsFromParams(j.Params)
		if err != nil {
			return migrate.JobRecord{}, fmt.Errorf("reading the sync's parameters: %w", err)
		}
		rec.DryRun = opts.DryRun
	case job.TypeScrub:
		percent, err := job.ScrubPercentFromParams(j.Params)
		if err != nil {
			return migrate.JobRecord{}, fmt.Errorf("reading the scrub's parameters: %w", err)
		}
		allBlocks, err := job.ScrubAllBlocksFromParams(j.Params)
		if err != nil {
			return migrate.JobRecord{}, fmt.Errorf("reading the scrub's parameters: %w", err)
		}
		rec.ScrubPercent, rec.AllBlocks = percent, allBlocks
	case job.TypeFix:
		path, err := job.FixPathFromParams(j.Params)
		if err != nil {
			return migrate.JobRecord{}, fmt.Errorf("reading the fix's parameters: %w", err)
		}
		rec.Path = path
	case job.TypeShareRelocation:
		var p job.ShareRelocationParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return migrate.JobRecord{}, fmt.Errorf("reading the share relocation's parameters: %w", err)
		}
		rec.Share, rec.To = p.Share, p.To
	}
	return rec, nil
}

func checklistChannels(svc *notify.Service) func(context.Context) ([]migrate.ChannelRecord, error) {
	return func(ctx context.Context) ([]migrate.ChannelRecord, error) {
		channels, err := svc.ListChannels(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]migrate.ChannelRecord, 0, len(channels))
		for _, ch := range channels {
			out = append(out, migrate.ChannelRecord{ID: ch.ID, Enabled: ch.Enabled, UpdatedAt: ch.UpdatedAt})
		}
		return out, nil
	}
}

func checklistSchedule(svc *ScheduleService) func(context.Context) (migrate.ScheduleRecord, error) {
	return func(ctx context.Context) (migrate.ScheduleRecord, error) {
		view, err := svc.Get(ctx)
		if err != nil {
			return migrate.ScheduleRecord{}, err
		}
		return migrate.ScheduleRecord{
			ChainStartTime: view.Chain.StartTime, WeeklyScrubDay: view.Chain.WeeklyScrubDay,
			Mover: view.Chain.Enabled[job.StepMover], Sync: view.Chain.Enabled[job.StepSync], Scrub: view.Chain.Enabled[job.StepScrub],
		}, nil
	}
}
