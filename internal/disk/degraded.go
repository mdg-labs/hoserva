package disk

import (
	"errors"
	"sync"
)

// ExpectedDisk is one disk hoservad expects to find present at boot (doc
// 02 §1, Q69, Q21): the identity every array-setup or disk-add job
// recorded for it, its role for the degraded-state report, and the
// mountpoint it is assigned.
type ExpectedDisk struct {
	Identity Identity
	Role     string // "data", "parity" or "cache"
	MountAt  string
}

// ReadinessCheck is one evaluation of a set of ExpectedDisk against the
// disks a Provider.List actually found (Q69): Ready is true only when
// every expected disk was matched by identity and, where both sides carry
// one, its filesystem UUID agreed too; Missing names the rest, in the
// order they were declared, for the degraded-state banner.
type ReadinessCheck struct {
	Ready   bool
	Missing []ExpectedDisk
	// WrongFilesystem holds every ExpectedDisk matched by identity (Q21)
	// whose recorded filesystem UUID does not match what evaluate found on
	// the matching disk (#388) — a replacement disk carrying the original
	// disk's serial/WWN (a cloned or reused drive, or one from the same
	// enclosure) but a different, or absent, filesystem. Distinct from
	// Missing: the disk is genuinely present. Reporting this slot ready
	// anyway is exactly what let hoservad's own startup mount call hang
	// waiting on a `/dev/disk/by-uuid/<uuid>` device that never appears,
	// past Type=notify's own TimeoutStartSec, restarting forever (#388).
	WrongFilesystem []ExpectedDisk
}

// evaluate matches expected against present by Identity.Matches (Q21),
// then — for every match where both sides carry a filesystem UUID —
// confirms it agrees (#388); Missing names every expected disk not found
// at all, WrongFilesystem every one found by identity but carrying a
// different filesystem than SQLite recorded for that slot.
func evaluate(expected []ExpectedDisk, present []Identity) ReadinessCheck {
	var missing, wrongFS []ExpectedDisk
	for _, e := range expected {
		var match *Identity
		for i := range present {
			if e.Identity.Matches(present[i]) {
				match = &present[i]
				break
			}
		}
		switch {
		case match == nil:
			missing = append(missing, e)
		case FSUUIDMismatch(e.Identity.FSUUID, match.FSUUID):
			wrongFS = append(wrongFS, e)
		}
	}
	return ReadinessCheck{Ready: len(missing) == 0 && len(wrongFS) == 0, Missing: missing, WrongFilesystem: wrongFS}
}

// ErrNothingToAcknowledge is StorageGate.Acknowledge's refusal when the
// gate is already ready — acknowledging a degraded state that does not
// exist would silently mask a future, real one, since Acknowledge does
// not expire.
var ErrNothingToAcknowledge = errors.New("disk: storage is not degraded, nothing to acknowledge")

// StorageGate is the boot-time readiness gate doc 02 §1 and Q69 describe:
// hoservad reaches hoserva-storage.target only once every ExpectedDisk is
// present by identity, or once a human explicitly acknowledges the
// degraded state through the API. It holds no timer of its own — nothing
// here polls; a caller (a future internal/api handler) calls Evaluate
// each time disk.Provider.List changes, e.g. on daemon start and on a
// udev-driven disk-arrival event.
type StorageGate struct {
	mu           sync.Mutex
	expected     []ExpectedDisk
	last         ReadinessCheck
	acknowledged bool
}

// NewStorageGate returns a StorageGate for expected, not yet evaluated
// (Ready() is false until the first Evaluate call).
func NewStorageGate(expected []ExpectedDisk) *StorageGate {
	return &StorageGate{expected: expected}
}

// Evaluate re-runs the readiness check against present (every currently
// enumerated disk's Identity) and returns it. A fresh Evaluate call that
// finds every expected disk present clears any earlier acknowledgement:
// once the array is genuinely whole again, a stale "degraded, proceed
// anyway" acknowledgement must not linger for the next disk that actually
// goes missing.
func (g *StorageGate) Evaluate(present []Identity) ReadinessCheck {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = evaluate(g.expected, present)
	if g.last.Ready {
		g.acknowledged = false
	}
	return g.last
}

// Ready reports whether hoservad may reach hoserva-storage.target: every
// expected disk was present at the last Evaluate, or the missing ones
// were explicitly acknowledged.
func (g *StorageGate) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last.Ready || g.acknowledged
}

// Missing returns the ExpectedDisk entries not found at the last
// Evaluate, for the degraded-state banner (Q69) — populated even once
// Acknowledge has been called, so the UI can keep showing what is still
// physically absent.
func (g *StorageGate) Missing() []ExpectedDisk {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ExpectedDisk(nil), g.last.Missing...)
}

// WrongFilesystem returns the ExpectedDisk entries matched by identity at
// the last Evaluate whose filesystem did not (#388) — populated even once
// Acknowledge has been called, mirroring Missing, so a caller can keep
// telling "physically absent" from "present, but not the disk that used
// to be here" apart.
func (g *StorageGate) WrongFilesystem() []ExpectedDisk {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ExpectedDisk(nil), g.last.WrongFilesystem...)
}

// Acknowledge records the user's explicit choice to proceed with a
// degraded pool (Q69's "once the user acknowledges the degraded state") —
// the API operation a future internal/api handler binds a POST to. It
// refuses when nothing is currently missing (ErrNothingToAcknowledge):
// there is no degraded state to acknowledge, and accepting the call
// anyway would let a stale acknowledgement outlive the situation it was
// about.
func (g *StorageGate) Acknowledge() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last.Ready {
		return ErrNothingToAcknowledge
	}
	g.acknowledged = true
	return nil
}
