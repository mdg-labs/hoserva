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
// storage down. cmd/hoservad's real implementation execs `systemctl
// poweroff`; every test here uses a fake (CLAUDE.md: "every
// system-touching subsystem sits behind a package interface with a
// scriptable fake").
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

// Shutdown runs ArraySequence.StopForShutdown and, only once it succeeds,
// powers the host off. StopForShutdown, never Stop (#387 finding 2): a
// UPS low-battery shutdown is not a user asking the array to stay stopped
// once power returns — persisting a new "stopped" state here would leave
// RestorePersistedMaintenance holding the array offline after the next
// ordinary boot. A persisted user `array stop` already in force is left
// exactly as it is. A failed stop leaves maintenance mode active
// (ArraySequence.stop's own doc comment) and never reaches PowerOff —
// powering off while a service refused to stop is exactly the data-loss
// scenario the stop sequence itself exists to prevent.
func (u UPSShutdown) Shutdown(ctx context.Context) error {
	if err := u.Array.StopForShutdown(ctx); err != nil {
		return fmt.Errorf("job: ups low-battery shutdown: stopping the array: %w", err)
	}
	if u.Power == nil {
		return nil
	}
	return u.Power.PowerOff(ctx)
}

// UPSController reacts to NUT's own upsmon notifications (Q77, doc 02
// §6): HandleNotify is what NOTIFYCMD's script calls, and also what
// SHUTDOWNCMD's own helper calls with UPSNotifyLowBattery — generating
// the upsmon.conf that names both is internal/config's job (nut.go);
// cmd/hoservad wires each script to this type over its own control
// socket, since both run as a separate process from the daemon holding
// this type's live Scheduler state. UPSController never polls a UPS
// itself (D1) — upsmon decides ONBATT/ONLINE/LOWBATT; this only reacts to
// that decision.
type UPSController struct {
	Scheduler *Scheduler
	Notifier  UPSNotifier
	Shutdown  ShutdownSequence

	mu            sync.Mutex
	batteryActive bool
	pausedJobIDs  []string

	// shutdownRun makes a low-battery shutdown collapse concurrent
	// callers onto one real run without latching a failure forever:
	// upsmon calls NOTIFYCMD(LOWBATT) and, independently, its own
	// SHUTDOWNCMD a few seconds later once FSD's FINALDELAY elapses (doc
	// 02 §6) — both reach handleLowBattery close together, and running
	// Array.Stop/PowerOff twice concurrently is exactly the unmount-
	// while-still-tearing-down race this package exists to prevent, so a
	// caller that finds a run already in flight waits for it and shares
	// its result rather than starting a second one. But a shutdown that
	// fails (a service refuses to stop, TestUPSShutdown_
	// ServiceRefusesToStop_NeverPowersOff's own case) must not block
	// every later outage's own LOWBATT forever behind that one stale
	// error — handleLowBattery clears shutdownRun once a run finishes,
	// success or failure alike, so the next LOWBATT starts a fresh run
	// and actually retries instead of riding the battery to zero on a
	// cached result.
	shutdownRun *shutdownRun
}

// shutdownRun is one in-flight or just-finished handleLowBattery run:
// concurrent callers that arrive while it is in flight block on done and
// then read err, exactly like sync.Once.Do's own callers would, but
// without sync.Once's permanent latch.
type shutdownRun struct {
	done chan struct{}
	err  error
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

	paused := c.Scheduler.PauseForBattery(ctx)
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
// the shutdown must run regardless. A caller that finds a run already in
// flight shares its result instead of starting a second one; once a run
// finishes, shutdownRun clears so the next LOWBATT — a later outage,
// after a prior run failed — actually retries rather than replaying a
// stale error (see UPSController's own doc comment on shutdownRun).
func (c *UPSController) handleLowBattery(ctx context.Context) error {
	c.mu.Lock()
	if run := c.shutdownRun; run != nil {
		c.mu.Unlock()
		<-run.done
		return run.err
	}
	run := &shutdownRun{done: make(chan struct{})}
	c.shutdownRun = run
	c.mu.Unlock()

	if c.Notifier != nil {
		c.Notifier.NotifyUPSBatteryLow(ctx)
	}
	var err error
	if c.Shutdown == nil {
		err = fmt.Errorf("job: low battery: no shutdown sequence configured")
	} else {
		err = c.Shutdown.Shutdown(ctx)
	}
	run.err = err
	close(run.done)

	c.mu.Lock()
	if c.shutdownRun == run {
		c.shutdownRun = nil
	}
	c.mu.Unlock()
	return err
}
