package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// dockerDaemonConfigPath is Q62/Q76's own managed Docker daemon config,
// relative to Generator.Root (production: /etc/docker/daemon.json).
const dockerDaemonConfigPath = "docker/daemon.json"

// dockerRestartPendingPath marks that ApplyDockerDataRoot has issued Stop
// for this move and does not yet have confirmation that Start succeeded.
// It exists purely as Hoserva's own bookkeeping — never a host file, never
// routed through Generator's manifest/Q76 unmanaged-file machinery — so a
// later call can tell "Docker was stopped by an earlier attempt of this
// exact move and needs finishing" apart from "Docker simply hasn't been
// started yet" (e.g. a fresh install, still waiting on
// hoserva-storage.target's own drop-in to start it for the first time):
// both look identical from daemon.json content and Docker's current
// Active() state alone, so neither can be inferred — only this file
// (written before Stop, removed only once Start is confirmed) tells them
// apart.
const dockerRestartPendingPath = "docker/.restart-pending"

// dockerDataRootDirMode is the permission ApplyDockerDataRoot creates the
// cache-side data-root directory with — root-only, matching Docker's own
// default ownership of /var/lib/docker.
const dockerDataRootDirMode = 0o711

// ErrDockerDataRootInUse is ApplyDockerDataRoot's refusal when the
// directory it would make Docker's data-root already holds entries that
// Docker's own daemon.json does not name as its data-root — a share's cache
// branch, say. Docker would run as root on whatever is in there, and the
// directory's owner and mode would be set by whoever created it.
var ErrDockerDataRootInUse = errors.New("config: docker data-root directory already holds files")

// DirMaker creates a directory tree. ApplyDockerDataRoot's data-root move
// is the one filesystem write it makes outside Generator.Root (the cache
// mount, not /etc), so it sits behind this small interface with a
// scriptable fake (doc 06 §2) instead of calling os.MkdirAll directly.
type DirMaker interface {
	MkdirAll(path string, perm os.FileMode) error
}

// OSDirMaker is the real DirMaker.
type OSDirMaker struct{}

func (OSDirMaker) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ServiceRestarter restarts Docker after its data-root moves (Q62, Q76):
// Active reports whether it is currently running — ApplyDockerDataRoot
// only ever restarts a Docker that was already up, never starts it ahead
// of hoserva-storage.target's own drop-in (Q69), which is what starts it
// the first time. Stop then Start go through whatever systemd abstraction
// the caller wires in (main.go uses disk.ServiceUnitController, the same
// Stop/Start every other managed service — Samba, NFS, NUT — already goes
// through), never a raw shell command (CLAUDE.md).
type ServiceRestarter interface {
	Active(ctx context.Context) (bool, error)
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// SystemdServiceRestarter is the real ServiceRestarter, querying and
// controlling Unit through systemctl.
type SystemdServiceRestarter struct {
	Unit   string // e.g. "docker.service"
	Runner disk.Runner
}

func (r SystemdServiceRestarter) Active(ctx context.Context) (bool, error) {
	out, err := r.Runner.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", r.Unit)
	if err != nil {
		return false, fmt.Errorf("config: checking %s state: %w", r.Unit, err)
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

func (r SystemdServiceRestarter) Stop(ctx context.Context) error {
	if _, err := r.Runner.Run(ctx, "systemctl", "stop", r.Unit); err != nil {
		return fmt.Errorf("config: stopping %s: %w", r.Unit, err)
	}
	return nil
}

func (r SystemdServiceRestarter) Start(ctx context.Context) error {
	if _, err := r.Runner.Run(ctx, "systemctl", "start", r.Unit); err != nil {
		return fmt.Errorf("config: starting %s: %w", r.Unit, err)
	}
	return nil
}

var _ ServiceRestarter = SystemdServiceRestarter{}

// dockerDaemonConfig is the subset of Docker's own daemon.json Hoserva
// generates (Q62). overlay2 is already the Engine's own default storage
// driver; it is set explicitly so a host whose old, unmanaged
// /etc/docker/daemon.json requested a different one is not silently left
// on it once Hoserva starts managing this file.
type dockerDaemonConfig struct {
	DataRoot      string `json:"data-root"`
	StorageDriver string `json:"storage-driver"`
}

// DockerRestartPending reports whether an earlier ApplyDockerDataRoot call
// issued Stop without a confirmed, matching Start — so a caller (Handler.
// ApplyHostConfig) can recognize a recovery is owed even when its own
// fresh inventory probe fails closed to DockerDataRootDefault precisely
// because Docker is down from that same earlier Stop (Q76's DockerErr
// fail-closed rule), which would otherwise make ApplyDockerDataRoot's own
// pending check unreachable: it never runs past DockerDataRootDefault's
// immediate no-op.
func (g *Generator) DockerRestartPending() (bool, error) {
	full, _, err := g.resolvePath(dockerRestartPendingPath)
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(full); statErr == nil {
		return true, nil
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("config: checking docker restart marker: %w", statErr)
	}
	return false, nil
}

// markDockerRestartPending records that Stop is about to be issued, before
// it runs — so a crash or failure anywhere after this point (including
// Stop itself never actually reaching Docker) leaves the marker for a
// later call to resolve, rather than losing track of an in-flight stop.
func (g *Generator) markDockerRestartPending() error {
	full, _, err := g.resolvePath(dockerRestartPendingPath)
	if err != nil {
		return err
	}
	if err := atomicWrite(full, []byte("pending\n"), 0o600, -1, false); err != nil {
		return fmt.Errorf("config: recording docker restart marker: %w", err)
	}
	return nil
}

// clearDockerRestartPending removes the marker once Start is confirmed (or
// once it's established Docker was never actually stopped, so there is
// nothing to finish).
func (g *Generator) clearDockerRestartPending() error {
	full, _, err := g.resolvePath(dockerRestartPendingPath)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: clearing docker restart marker: %w", err)
	}
	return nil
}

// CanApplyDockerDataRoot reports whether ApplyDockerDataRoot would refuse
// dataRoot's move with ErrExistingHostFile, without writing anything — so
// a caller batching several durable writes in one request (ApplyHostConfig:
// share imports, host-file decisions, then the data-root move) can check
// this first and refuse the whole request before committing any of them,
// rather than discovering the refusal only after the others are already
// persisted. A no-op, like ApplyDockerDataRoot itself, whenever dataRoot is
// DockerDataRootDefault.
func (g *Generator) CanApplyDockerDataRoot(ctx context.Context, dataRoot string) error {
	if dataRoot == DockerDataRootDefault {
		return nil
	}
	if keep, err := g.keepsLegacyDockerDataRoot(dataRoot); err != nil || keep {
		return err
	}
	if err := g.checkDockerDataRootUnused(dataRoot); err != nil {
		return err
	}
	return g.CanWrite(ctx, dockerDaemonConfigPath)
}

// checkDockerDataRootUnused refuses dataRoot when it exists with entries and
// the managed daemon.json does not already name it: an earlier, completed
// move of this same data-root leaves Docker's own entries there, and
// re-applying it must stay a no-op. A directory that cannot be read is
// refused too, never taken as empty.
func (g *Generator) checkDockerDataRootUnused(dataRoot string) error {
	entries, err := os.ReadDir(dataRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: reading docker data-root %s: %w", dataRoot, err)
	}
	if len(entries) == 0 {
		return nil
	}
	configured, err := g.configuredDockerDataRoot()
	if err != nil {
		return err
	}
	if configured == dataRoot {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDockerDataRootInUse, dataRoot)
}

// keepsLegacyDockerDataRoot reports whether dataRoot is the current cache
// data-root while the managed daemon.json already names the legacy one: an
// install moved there by an earlier version keeps it, so the move is not
// repeated.
func (g *Generator) keepsLegacyDockerDataRoot(dataRoot string) (bool, error) {
	if dataRoot != DockerDataRootCache {
		return false, nil
	}
	configured, err := g.configuredDockerDataRoot()
	if err != nil {
		return false, err
	}
	return configured == DockerDataRootCacheLegacy, nil
}

// EffectiveDockerDataRoot is the data-root ApplyDockerDataRoot leaves
// configured for dataRoot: the legacy cache path when an install already
// moved there, dataRoot otherwise.
func (g *Generator) EffectiveDockerDataRoot(dataRoot string) (string, error) {
	keep, err := g.keepsLegacyDockerDataRoot(dataRoot)
	if err != nil {
		return "", err
	}
	if keep {
		return DockerDataRootCacheLegacy, nil
	}
	return dataRoot, nil
}

// configuredDockerDataRoot is the data-root the managed daemon.json names,
// or "" when there is none or it names none.
func (g *Generator) configuredDockerDataRoot() (string, error) {
	full, _, err := g.resolvePath(dockerDaemonConfigPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("config: reading docker daemon.json: %w", err)
	}
	var cfg dockerDaemonConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", nil
	}
	return cfg.DataRoot, nil
}

// ApplyDockerDataRoot performs the move DockerDataRoot only decides:
// writes /etc/docker/daemon.json, creates dataRoot, and restarts Docker if
// it is already running, so it picks up the new location. It is a no-op
// whenever dataRoot is DockerDataRootDefault and no restart is owed —
// nothing to move, matching DockerDataRoot's own refusal to move without
// an accepted decision, a cache disk and an empty Engine (Q76). An
// existing, unmanaged /etc/docker/daemon.json — Docker already configured
// by hand before Hoserva was installed — refuses via ErrExistingHostFile
// exactly like Generator's other managed host files (D4, Q76): this call
// never overwrites configuration it did not write. dirs and restart may
// be nil; dirs defaults to OSDirMaker, and a nil restart only skips the
// restart step (the config and directory are still written).
//
// The pending-restart check runs before the DockerDataRootDefault no-op,
// not after: a caller's own fresh inventory probe can itself fail
// precisely because Docker is down from an earlier call's Stop, and Q76's
// DockerErr fail-closed rule then makes that caller recompute
// DockerDataRootDefault on this very retry (Handler.ApplyHostConfig
// guards against this too, via DockerRestartPending, but this function
// must not depend on getting a correct dataRoot to finish its own
// half-done work). A pending restart always finishes directly with Start
// — never a second Stop — rather than being skipped as "was never
// running" (which Docker's live Active() state alone cannot rule out: a
// fresh install with Docker genuinely not started yet looks identical to
// that from Active() and daemon.json content both).
func (g *Generator) ApplyDockerDataRoot(ctx context.Context, dataRoot string, dirs DirMaker, restart ServiceRestarter, revision int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if restart != nil {
		pending, err := g.DockerRestartPending()
		if err != nil {
			return err
		}
		if pending {
			if err := restart.Start(ctx); err != nil {
				return fmt.Errorf("config: starting docker after its data-root move: %w", err)
			}
			return g.clearDockerRestartPending()
		}
	}

	if dataRoot == DockerDataRootDefault {
		return nil
	}
	if keep, err := g.keepsLegacyDockerDataRoot(dataRoot); err != nil || keep {
		return err
	}
	if err := g.checkDockerDataRootUnused(dataRoot); err != nil {
		return err
	}
	if dirs == nil {
		dirs = OSDirMaker{}
	}
	if err := dirs.MkdirAll(dataRoot, dockerDataRootDirMode); err != nil {
		return fmt.Errorf("config: creating docker data-root %s: %w", dataRoot, err)
	}

	body, err := json.MarshalIndent(dockerDaemonConfig{DataRoot: dataRoot, StorageDriver: "overlay2"}, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding docker daemon.json: %w", err)
	}
	if err := g.Write(ctx, File{Path: dockerDaemonConfigPath, Body: body, RawBody: true}, revision, now); err != nil {
		return fmt.Errorf("config: writing docker daemon.json: %w", err)
	}

	if restart == nil {
		return nil
	}

	active, err := restart.Active(ctx)
	if err != nil {
		return fmt.Errorf("config: checking whether docker is running: %w", err)
	}
	if !active {
		return nil
	}
	if err := g.markDockerRestartPending(); err != nil {
		return err
	}
	if err := restart.Stop(ctx); err != nil {
		// Docker was never actually stopped — nothing to finish later.
		if clearErr := g.clearDockerRestartPending(); clearErr != nil {
			return fmt.Errorf("config: stopping docker before its data-root move: %w (clearing restart marker: %v)", err, clearErr)
		}
		return fmt.Errorf("config: stopping docker before its data-root move: %w", err)
	}
	if err := restart.Start(ctx); err != nil {
		// Marker stays: the next call must finish this, never re-stop.
		return fmt.Errorf("config: starting docker after its data-root move: %w", err)
	}
	return g.clearDockerRestartPending()
}
