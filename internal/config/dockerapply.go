package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// dockerDaemonConfigPath is Q62/Q76's own managed Docker daemon config,
// relative to Generator.Root (production: /etc/docker/daemon.json).
const dockerDaemonConfigPath = "docker/daemon.json"

// dockerDataRootDirMode is the permission ApplyDockerDataRoot creates the
// cache-side data-root directory with — root-only, matching Docker's own
// default ownership of /var/lib/docker.
const dockerDataRootDirMode = 0o711

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
	return g.CanWrite(ctx, dockerDaemonConfigPath)
}

// ApplyDockerDataRoot performs the move DockerDataRoot only decides:
// writes /etc/docker/daemon.json, creates dataRoot, and restarts Docker if
// it is already running, so it picks up the new location. It is a no-op
// whenever dataRoot is DockerDataRootDefault — nothing to move, matching
// DockerDataRoot's own refusal to move without an accepted decision, a
// cache disk and an empty Engine (Q76). An existing, unmanaged
// /etc/docker/daemon.json — Docker already configured by hand before
// Hoserva was installed — refuses via ErrExistingHostFile exactly like
// Generator's other managed host files (D4, Q76): this call never
// overwrites configuration it did not write. dirs and restart may be nil;
// dirs defaults to OSDirMaker, and a nil restart only skips the restart
// step (the config and directory are still written). A retry that finds
// daemon.json already at dataRoot but Docker inactive treats that as its
// own earlier Stop having outlived a failed Start, and finishes the
// restart rather than skipping it as "was never running."
func (g *Generator) ApplyDockerDataRoot(ctx context.Context, dataRoot string, dirs DirMaker, restart ServiceRestarter, revision int, now time.Time) error {
	if dataRoot == DockerDataRootDefault {
		return nil
	}
	if err := ctx.Err(); err != nil {
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

	// Read what's on disk before Write replaces it (finding: a retry after
	// Stop succeeded but Start failed must not silently skip recovering
	// that). Write is idempotent per path, so an already-identical
	// daemon.json means an earlier call already applied this exact move —
	// if Docker is inactive now, that earlier call is what stopped it, and
	// this one must finish starting it back up rather than treating
	// "currently inactive" as "was never running."
	full, _, err := g.resolvePath(dockerDaemonConfigPath)
	if err != nil {
		return err
	}
	alreadyApplied := false
	if existing, readErr := os.ReadFile(full); readErr == nil {
		alreadyApplied = string(existing) == string(body)
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("config: checking docker daemon.json: %w", readErr)
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
		if !alreadyApplied {
			return nil
		}
		if err := restart.Start(ctx); err != nil {
			return fmt.Errorf("config: starting docker after its data-root move: %w", err)
		}
		return nil
	}
	if err := restart.Stop(ctx); err != nil {
		return fmt.Errorf("config: stopping docker before its data-root move: %w", err)
	}
	if err := restart.Start(ctx); err != nil {
		return fmt.Errorf("config: starting docker after its data-root move: %w", err)
	}
	return nil
}
