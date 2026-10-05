package api

import (
	"context"
	"log"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

func optTime(t time.Time) apiv1.OptDateTime {
	if t.IsZero() {
		return apiv1.OptDateTime{}
	}
	return apiv1.NewOptDateTime(t)
}

// ChecklistActor is who an acknowledgement is recorded as: the signed-in
// user, or "local" for the daemon's own socket, whose identity has no name.
func ChecklistActor(ctx context.Context) string {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Username == "" {
		return "local"
	}
	return p.Username
}

// MigrationChecklistItemToAPI is the API's form of one checklist item; the mock
// API uses it too, so the two answer alike.
func MigrationChecklistItemToAPI(it migrate.ChecklistItem) apiv1.MigrationChecklistItem {
	out := apiv1.MigrationChecklistItem{
		ID: apiv1.MigrationChecklistItemId(it.ID), Status: apiv1.MigrationChecklistItemStatus(it.Status),
		Acknowledgeable: it.Acknowledgeable, DoneAt: optTime(it.DoneAt), JobId: optString(it.JobID),
	}
	if it.Ack != nil {
		out.AcknowledgedBy, out.AcknowledgedAt = optString(it.Ack.By), optTime(it.Ack.At)
	}
	if n := it.Notifications; n != nil {
		agents := append([]string{}, n.Agents...)
		out.Notifications = apiv1.NewOptMigrationChecklistNotifications(apiv1.MigrationChecklistNotifications{Channels: int32(n.Channels), Tested: int32(n.Tested), Agents: agents})
	}
	if s := it.Schedules; s != nil {
		o := apiv1.MigrationChecklistOffers{MoverCron: optString(s.Offers.MoverCron), MoverTime: optString(s.Offers.MoverTime), SpindownDelay: optString(s.Offers.SpindownDelay)}
		if p := s.Offers.ParityCheck; p != nil {
			o.ParityCheck = apiv1.NewOptMigrationChecklistParityCheck(apiv1.MigrationChecklistParityCheck{
				Mode: optString(p.Mode), Hour: optString(p.Hour), DayOfMonth: optString(p.DayOfMonth), Day: optString(p.Day),
				Month: optString(p.Month), Frequency: optString(p.Frequency), Correcting: p.Correcting,
			})
			o.ScrubReportOnly = apiv1.NewOptBool(s.Offers.ScrubReportOnly)
		}
		out.Schedules = apiv1.NewOptMigrationChecklistSchedules(apiv1.MigrationChecklistSchedules{
			Mover: s.Mover, Sync: s.Sync, Scrub: s.Scrub, ChainStartTime: s.ChainStartTime, WeeklyScrubDay: apiv1.Weekday(s.WeeklyScrubDay), Offers: o,
		})
	}
	if it.ID == migrate.ItemUserScripts {
		out.Scripts = make([]apiv1.MigrationChecklistScript, 0, len(it.Scripts))
		for _, sc := range it.Scripts {
			out.Scripts = append(out.Scripts, apiv1.MigrationChecklistScript{Name: sc.Name, Schedule: optString(sc.Schedule)})
		}
	}
	return out
}

// MigrationChecklistToAPI is the API's form of the checklist.
func MigrationChecklistToAPI(c migrate.Checklist) *apiv1.MigrationChecklist {
	out := &apiv1.MigrationChecklist{Finished: c.Finished, FinishedAt: optTime(c.FinishedAt), Items: make([]apiv1.MigrationChecklistItem, 0, len(c.Items))}
	for _, it := range c.Items {
		out.Items = append(out.Items, MigrationChecklistItemToAPI(it))
	}
	return out
}

// GetMigrationChecklist returns Phase D's closing steps, each derived from the
// records that show it (doc 05 §4 steps 18 and 21 to 25).
func (h *Handler) GetMigrationChecklist(ctx context.Context) (*apiv1.MigrationChecklist, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	c, err := h.Migration.Checklist(ctx)
	if err != nil {
		return nil, migrateError(err)
	}
	return MigrationChecklistToAPI(c), nil
}

// AcknowledgeMigrationChecklistItem records the user's acknowledgement of an
// item no record can show.
func (h *Handler) AcknowledgeMigrationChecklistItem(ctx context.Context, params apiv1.AcknowledgeMigrationChecklistItemParams) (*apiv1.MigrationChecklistItem, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	it, err := h.Migration.AcknowledgeChecklistItem(ctx, migrate.ChecklistItemID(params.Item), ChecklistActor(ctx))
	if err != nil {
		return nil, migrateError(err)
	}
	out := MigrationChecklistItemToAPI(it)
	return &out, nil
}

// recordChannelTest tells the checklist a channel's test succeeded. A record
// that cannot be written leaves the notifications item undone, and the test's
// own result is still the answer.
func (h *Handler) recordChannelTest(ctx context.Context, channelID string) {
	if h.Migration == nil {
		return
	}
	if err := h.Migration.RecordChannelTest(context.WithoutCancel(ctx), channelID); err != nil {
		log.Printf("hoservad: recording the successful test of notification channel %s for the migration checklist: %v", channelID, err)
	}
}
