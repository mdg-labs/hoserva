package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// UPSNotifyType is one of NUT's own upsmon.conf(5) NOTIFYTYPE values —
// only the three Q77 gives Hoserva a defined reaction to. Every other
// type (COMMBAD, REPLBATT, NOCOMM, ...) is a HandleNotify no-op, left to
// NUT's own default SYSLOG-only logging: this package never reimplements
// NUT's own battery or communication monitoring (D1), it only reacts to
// what upsmon already decided.
type UPSNotifyType string

const (
	UPSNotifyOnBattery  UPSNotifyType = "ONBATT"
	UPSNotifyOnLine     UPSNotifyType = "ONLINE"
	UPSNotifyLowBattery UPSNotifyType = "LOWBATT"
)

// UPSNotifier delivers doc 03 §8.3's two UPS events. internal/notify
// implements this without this package importing it — the same split
// chain.go's ChainNotifier already established for the guard-blocked
// notification.
type UPSNotifier interface {
	NotifyUPSOnBattery(ctx context.Context)
	NotifyUPSBatteryLow(ctx context.Context)
}

// ShutdownSequence is the clean shutdown Q70 and Q77 call for once low
// battery is reported: every running job checkpointed or marked
// interrupted, storage torn down safely, then the host powered off.
// UPSShutdown (below) is this package's own composition of ArraySequence
// and PowerOff; every UPSController test uses a fake.
type ShutdownSequence interface {
	Shutdown(ctx context.Context) error
}

// PowerOff issues the final step of the low-battery shutdown sequence,
// once ArraySequence.Stop has already checkpointed every job and torn
// storage down. A real implementation execs `systemctl poweroff`
// (cmd/hoservad, outside this issue's declared scope); every test here
// uses a fake (CLAUDE.md: "every system-touching subsystem sits behind a
// package interface with a scriptable fake").
type PowerOff interface {
	PowerOff(ctx context.Context) error
}

// UPSShutdown is Q70 and Q77's own composition: ArraySequence's stop
// sequence (maintenance mode, drain, services, unmounts), then PowerOff.
// It is the concrete ShutdownSequence UPSController.HandleNotify(LOWBATT)
// calls.
type UPSShutdown struct {
	Array ArraySequence
	Power PowerOff
}

// Shutdown runs ArraySequence.Stop and, only once it succeeds, powers the
// host off. A failed Stop leaves maintenance mode active (ArraySequence
// .Stop's own doc comment) and never reaches PowerOff — powering off
// while a service refused to stop is exactly the data-loss scenario Stop
// itself exists to prevent.
func (u UPSShutdown) Shutdown(ctx context.Context) error {
	if err := u.Array.Stop(ctx); err != nil {
		return fmt.Errorf("job: ups low-battery shutdown: stopping the array: %w", err)
	}
	if u.Power == nil {
		return nil
	}
	return u.Power.PowerOff(ctx)
}

// UPSController reacts to NUT's own upsmon notifications (Q77, doc 02
// §6): HandleNotify is what a NOTIFYCMD script calls — generating the
// upsmon.conf that names one is internal/config's job (nut.go); the
// script itself, and wiring it to this type, is cmd/hoservad's, outside
// this issue's declared scope. UPSController never polls a UPS itself
// (D1) — upsmon decides ONBATT/ONLINE/LOWBATT; this only reacts to that
// decision.
type UPSController struct {
	Scheduler *Scheduler
	Notifier  UPSNotifier
	Shutdown  ShutdownSequence

	mu            sync.Mutex
	batteryActive bool
	pausedJobIDs  []string
}

// HandleNotify reacts to one upsmon notification. A type outside
// UPSNotifyOnBattery/OnLine/LowBattery is a no-op — NUT's own default
// logging already covers it, and Q77 gives Hoserva no defined reaction
// to it.
func (c *UPSController) HandleNotify(ctx context.Context, notifyType UPSNotifyType) error {
	switch notifyType {
	case UPSNotifyOnBattery:
		return c.handleOnBattery(ctx)
	case UPSNotifyOnLine:
		return c.handleOnLine(ctx)
	case UPSNotifyLowBattery:
		return c.handleLowBattery(ctx)
	default:
		return nil
	}
}

// handleOnBattery is Q77's on-battery reaction: notify, pause the mover,
// hold scheduled syncs. Idempotent — a repeated ONBATT (NUT itself can
// send more than one before ONLINE) is a no-op past the first.
func (c *UPSController) handleOnBattery(ctx context.Context) error {
	c.mu.Lock()
	if c.batteryActive {
		c.mu.Unlock()
		return nil
	}
	c.batteryActive = true
	c.mu.Unlock()

	paused := c.Scheduler.PauseForBattery()
	c.mu.Lock()
	c.pausedJobIDs = paused
	c.mu.Unlock()

	if c.Notifier != nil {
		c.Notifier.NotifyUPSOnBattery(ctx)
	}
	return nil
}

// handleOnLine is Q77's power-restored reaction: both the mover pause
// and the sync hold reverse, and the mover job PauseForBattery stopped
// (if any) resumes automatically from its own checkpoint — an
// intentional exception to Q29's "resumed only by explicit user action":
// that rule is about a restart or `array stop` never silently
// reconstructing lost work, not about this deliberate, documented
// on-battery/power-restored pair. There is no routed notification for
// power returning — doc 03 §8.3's event catalog has none.
func (c *UPSController) handleOnLine(ctx context.Context) error {
	c.mu.Lock()
	if !c.batteryActive {
		c.mu.Unlock()
		return nil
	}
	c.batteryActive = false
	paused := c.pausedJobIDs
	c.pausedJobIDs = nil
	c.mu.Unlock()

	c.Scheduler.ResumeFromBattery()

	var errs []error
	for _, id := range paused {
		if _, err := c.Scheduler.Resume(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("job: resuming mover job %s after the battery hold: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// handleLowBattery is Q77's low-battery reaction: notify, then run the
// clean shutdown sequence — Q70's own checkpoint-everything-then-tear-
// down-storage ordering, plus powering the host off. It never checks
// batteryActive: LOWBATT can arrive without ONBATT ever having been seen
// (a very short outage, or upsmon starting up already on battery), and
// the shutdown must run regardless.
func (c *UPSController) handleLowBattery(ctx context.Context) error {
	if c.Notifier != nil {
		c.Notifier.NotifyUPSBatteryLow(ctx)
	}
	if c.Shutdown == nil {
		return fmt.Errorf("job: low battery: no shutdown sequence configured")
	}
	return c.Shutdown.Shutdown(ctx)
}
