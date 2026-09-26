package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// newReloadSignal registers this process's own SIGHUP handling at the top
// of run(), before any of its own IO (#372 finding 2), so a disk arriving
// at any later point during startup is queued in the returned channel
// instead of taking Go's default SIGHUP action, which terminates the
// process outright: systemd reads that exit as a clean stop, so
// Restart=on-failure never brings hoservad back, and the disk that
// triggered it would otherwise be lost until an unrelated trigger or a
// reboot. installReloadHandler is what actually drains this channel, once
// enough of startup has run to build the rebuild it needs.
func newReloadSignal() chan os.Signal {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	return sighup
}

// installReloadHandler re-runs rebuild every time hoservad receives
// SIGHUP (doc 02 §1, Q69: Samba, NFS, Docker and libvirt start only once
// hoservad reaches hoserva-storage.target, and a disk that arrives after
// boot has to reach that same gate without waiting for a reboot or an
// unrelated share edit). packaging/debian/hoserva-storage.rules fires
// exactly this signal — via `systemctl --no-block reload hoserva.service`,
// which packaging/debian/hoserva.service's own
// ExecReload=/bin/kill -HUP $MAINPID turns into this process's own
// SIGHUP — whenever udev sees a block device with a filesystem UUID
// appear: a disk reseated, or one that returns after being missing at
// boot. rebuild (rebuildArraySequence) only re-lists disks
// (disk.Provider.List, which reads udev's own cache,
// internal/disk/discovery.go) and re-evaluates disk.StorageGate; it never
// reads a data disk's own content, so this stays event-driven, not the
// kind of timer CLAUDE.md forbids — there is no poll here at all, only a
// reaction to the "add" event a returning disk already raises on its own.
//
// It drains any signal already queued in sighup and then runs one
// rebuild unconditionally, before starting its own goroutine (#372
// finding 2): systemd merges a `reload` that arrives while
// hoserva.service's own start job is still running into that job and
// never runs ExecReload at all, so a disk that arrives before this
// process's own READY=1 can leave nothing queued in sighup to drain; a
// disk that arrives after READY=1 but before this call is genuinely
// queued (sighup exists from newReloadSignal, called at the very start
// of startup specifically so nothing in that window is lost to Go's
// default SIGHUP action) and would otherwise sit unhandled until some
// later, unrelated SIGHUP. Either way, this one guaranteed rebuild — run
// only once main.go has enough built to call rebuild at all, always
// after READY=1 — re-evaluates it.
//
// The handler goroutine stops itself once ctx is done, so nothing here
// outlives cmd/hoservad's own lifetime.
func installReloadHandler(ctx context.Context, sighup chan os.Signal, rebuild func(context.Context) error) {
	select {
	case <-sighup:
	default:
	}
	if err := rebuild(ctx); err != nil {
		log.Printf("hoservad: re-evaluating storage gate at startup: %v", err)
	}
	go func() {
		defer signal.Stop(sighup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighup:
				if err := rebuild(ctx); err != nil {
					log.Printf("hoservad: re-evaluating storage gate after SIGHUP: %v", err)
				}
			}
		}
	}()
}
