package main

import (
	"context"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

// mockChecklist is the post-migration checklist's record in this mock: the
// acknowledgements and the channel tests that succeeded. Like production's it
// outlives forgetMigration.
type mockChecklist struct {
	mu  sync.Mutex
	rec migrate.ChecklistRecord
}

func (c *mockChecklist) snapshot() migrate.ChecklistRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := migrate.ChecklistRecord{Acks: map[migrate.ChecklistItemID]migrate.ChecklistAck{}, ChannelTests: map[string]time.Time{}}
	for k, v := range c.rec.Acks {
		out.Acks[k] = v
	}
	for k, v := range c.rec.ChannelTests {
		out.ChannelTests[k] = v
	}
	return out
}

func optNilTime(o apiv1.OptNilDateTime) time.Time {
	v, _ := o.Get()
	return v
}

// mockChecklistSources reads the records this mock keeps: its jobs, which carry
// no parameters but a fix job's path (so none is a full scrub or the appdata
// relocation, and none is a dry run), when its migration finished, its notification channels and its
// schedule.
func (h *handler) mockChecklistSources() migrate.ChecklistSources {
	return migrate.ChecklistSources{
		Finished: func(context.Context) (time.Time, bool, error) {
			h.migration.mu.Lock()
			defer h.migration.mu.Unlock()
			return h.migration.finishedAt, !h.migration.finishedAt.IsZero(), nil
		},
		Jobs: func(_ context.Context, jobType string, visit func(migrate.JobRecord) bool) error {
			h.mu.Lock()
			var recs []migrate.JobRecord
			for _, j := range h.jobs {
				if string(j.Type) == jobType && j.Status == apiv1.JobStatusSucceeded {
					recs = append(recs, migrate.JobRecord{ID: j.ID.String(), CreatedAt: j.CreatedAt, StartedAt: optNilTime(j.StartedAt), FinishedAt: optNilTime(j.FinishedAt), Path: h.fixPaths[j.ID]})
				}
			}
			h.mu.Unlock()
			sort.Slice(recs, func(a, b int) bool {
				if !recs[a].CreatedAt.Equal(recs[b].CreatedAt) {
					return recs[a].CreatedAt.Before(recs[b].CreatedAt)
				}
				return recs[a].ID < recs[b].ID
			})
			for _, r := range recs {
				if visit(r) {
					return nil
				}
			}
			return nil
		},
		Channels: func(context.Context) ([]migrate.ChannelRecord, error) {
			h.notifyMu.Lock()
			defer h.notifyMu.Unlock()
			var out []migrate.ChannelRecord
			for _, ch := range h.channels {
				out = append(out, migrate.ChannelRecord{ID: ch.ID.String(), Enabled: ch.Enabled, UpdatedAt: ch.UpdatedAt})
			}
			return out, nil
		},
		Schedule: func(context.Context) (migrate.ScheduleRecord, error) {
			h.notifyMu.Lock()
			defer h.notifyMu.Unlock()
			c := h.schedules.Chain
			rec := migrate.ScheduleRecord{ChainStartTime: c.StartTime, WeeklyScrubDay: int(c.WeeklyScrubDay)}
			for _, st := range c.Steps {
				switch st.ID {
				case apiv1.MaintenanceChainStepIdMover:
					rec.Mover = st.Enabled
				case apiv1.MaintenanceChainStepIdSync:
					rec.Sync = st.Enabled
				case apiv1.MaintenanceChainStepIdScrub:
					rec.Scrub = st.Enabled
				}
			}
			return rec, nil
		},
	}
}

func (h *handler) buildMockChecklist(ctx context.Context) (migrate.Checklist, migrate.ChecklistFacts, migrate.ChecklistRecord, *migrate.Report, error) {
	facts, err := h.mockChecklistSources().Facts(ctx)
	if err != nil {
		return migrate.Checklist{}, facts, migrate.ChecklistRecord{}, nil, err
	}
	h.migration.mu.Lock()
	report := h.migration.report
	h.migration.mu.Unlock()
	rec := h.migration.checklist.snapshot()
	return migrate.BuildChecklist(facts, rec, report), facts, rec, report, nil
}

// GetMigrationChecklist answers as production's does: the checklist applies
// only once the migration has a finished stamp, which the point of no return
// writes and nothing else does, and every item is derived from the mock's own records by the
// same code.
func (h *handler) GetMigrationChecklist(ctx context.Context) (*apiv1.MigrationChecklist, error) {
	c, _, _, _, err := h.buildMockChecklist(ctx)
	if err != nil {
		return nil, mockMigrateError(err)
	}
	return api.MigrationChecklistToAPI(c), nil
}

// AcknowledgeMigrationChecklistItem accepts and refuses exactly what
// production's does, through the same rules.
func (h *handler) AcknowledgeMigrationChecklistItem(ctx context.Context, params apiv1.AcknowledgeMigrationChecklistItemParams) (*apiv1.MigrationChecklistItem, error) {
	cl := &h.migration.checklist
	h.migration.checklistMu.Lock()
	defer h.migration.checklistMu.Unlock()
	c, facts, rec, report, err := h.buildMockChecklist(ctx)
	if err != nil {
		return nil, mockMigrateError(err)
	}
	id := migrate.ChecklistItemID(params.Item)
	ack, isNew, err := c.Acknowledge(id, api.ChecklistActor(ctx), time.Now().UTC(), facts)
	if err != nil {
		return nil, mockMigrateError(err)
	}
	if isNew {
		cl.mu.Lock()
		if cl.rec.Acks == nil {
			cl.rec.Acks = map[migrate.ChecklistItemID]migrate.ChecklistAck{}
		}
		cl.rec.Acks[id] = ack
		cl.mu.Unlock()
		rec.Acks[id] = ack
	}
	out := api.MigrationChecklistItemToAPI(*migrate.BuildChecklist(facts, rec, report).Item(id))
	return &out, nil
}

// recordChannelTest notes that a channel's test succeeded now, as production's
// handler does.
func (c *mockChecklist) recordChannelTest(channelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.ChannelTests == nil {
		c.rec.ChannelTests = map[string]time.Time{}
	}
	c.rec.ChannelTests[channelID] = time.Now().UTC()
}
