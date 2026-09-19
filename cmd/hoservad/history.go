package main

import (
	"context"
	"database/sql"
	"log"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// wireDaemonHistory constructs spin-event persistence from the daemon
// database and attaches it to handler so GET /disks/wake-events reads
// SQLite instead of always returning empty lists (#192).
func wireDaemonHistory(db *sql.DB, handler *api.Handler) *store.History {
	history := store.NewHistory(db)
	handler.History = history
	return history
}

// persistingDiskProvider wraps LinuxProvider so every spin-state
// transition LinuxProvider's in-memory SpinEventLog already observed
// from a SMART poll or spindown is also written through
// store.History.RecordSpinEvent (Q32) — recording stays a side effect of
// polls that already respect standby, never a dedicated wake probe.
type persistingDiskProvider struct {
	inner   *disk.LinuxProvider
	history *store.History
}

func newPersistingDiskProvider(inner *disk.LinuxProvider, history *store.History) *persistingDiskProvider {
	return &persistingDiskProvider{inner: inner, history: history}
}

func (p *persistingDiskProvider) List(ctx context.Context) ([]disk.Disk, error) {
	return p.inner.List(ctx)
}

func (p *persistingDiskProvider) SMART(ctx context.Context, dev string, mode disk.SMARTPollMode) (disk.SMARTReport, error) {
	before := len(p.inner.Events.Events(dev))
	report, err := p.inner.SMART(ctx, dev, mode)
	if err == nil {
		p.persistNewEvents(ctx, dev, before)
	}
	return report, err
}

func (p *persistingDiskProvider) Spindown(ctx context.Context, dev string) error {
	before := len(p.inner.Events.Events(dev))
	err := p.inner.Spindown(ctx, dev)
	if err == nil {
		p.persistNewEvents(ctx, dev, before)
	}
	return err
}

func (p *persistingDiskProvider) Format(ctx context.Context, dev string, fs disk.FilesystemType) error {
	return p.inner.Format(ctx, dev, fs)
}

func (p *persistingDiskProvider) persistNewEvents(ctx context.Context, dev string, before int) {
	events := p.inner.Events.Events(dev)
	for i := before; i < len(events); i++ {
		ev := events[i]
		if err := p.history.RecordSpinEvent(ctx, ev.Device, ev.From.String(), ev.To.String(), ev.At); err != nil {
			log.Printf("hoservad: persisting spin event for %s: %v", dev, err)
		}
	}
}

var _ disk.Provider = (*persistingDiskProvider)(nil)
