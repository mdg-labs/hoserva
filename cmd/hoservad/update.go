package main

import (
	"context"
	"database/sql"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/update"
)

// runApplyVerifiedUpdate is hoservad --apply-verified-update: the
// transient unit's payload (Q67). It must not open the live database
// before ApplyVerifiedUpdate — rollback restores a snapshot over that
// file, and a live connection would lose the restored rows.
func runApplyVerifiedUpdate(cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return update.ApplyVerifiedUpdate(ctx, cfg.applyVerifiedUpdate, filepath.Join(cfg.stateDir, "hoserva.db"), nil, nil)
}

func packageVersion(ctx context.Context, runner disk.Runner, name string) string {
	out, err := runner.Run(ctx, "dpkg-query", "-W", "-f", "${Version}", name)
	if err != nil {
		return "dev"
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "dev"
	}
	return v
}

func newBackupService(ctx context.Context, cfg config, db *sql.DB, machineKey *auth.MachineKey, recipient *backup.Recipient, settings *api.SettingsService, runner disk.Runner) *backup.Service {
	configRoot := cfg.configRoot
	if configRoot == "" {
		configRoot = "/etc"
	}
	return &backup.Service{
		DB:    db,
		Paths: backup.DefaultPaths(cfg.stateDir, filepath.Join(configRoot, "hoserva")),
		Destinations: []backup.Destination{{
			ID:      "boot",
			Path:    filepath.Join(cfg.stateDir, "backups"),
			Enabled: true,
			Retention: backup.Retention{
				Daily:   backup.DefaultRetentionDaily,
				Weekly:  backup.DefaultRetentionWeekly,
				Monthly: backup.DefaultRetentionMonthly,
			},
		}},
		Secrets:   &backup.ServiceSecretSource{BackupPassphraseFn: settings.BackupPassphrase},
		Cipher:    machineKey,
		Recipient: recipient,
		Version:   packageVersion(ctx, runner, "hoserva"),
	}
}

// wireBackup connects backupService — built once in run() the same way
// the nightly maintenance chain (scheduleRunner.Backup) and the
// pre-self-update backup (newUpdateEngine) already use it — to
// handler.Backup, so POST /config/export and POST /config/import
// (ExportConfig/ImportConfig, #269) stop 501ing. Kept as its own
// function, following wireAcknowledgeDegraded's pattern (array.go), so a
// test can call exactly what main.go calls rather than a hand copy of
// the assignment.
func wireBackup(handler *api.Handler, backupService *backup.Service) {
	handler.Backup = backupService
}

// updateShutdownLookup adapts the daemon's current job.ArraySequence to
// update.Shutdown, resolved at the moment Engine.Reboot actually calls
// Stop rather than once at daemon construction (#263) — the same
// live-array-creation staleness UPS shutdown has (upscontrol.go's
// upsShutdownLookup). Unlike the UPS path there is no scheduler fallback:
// with no array ever configured, Reboot has nothing to stop, exactly
// newUpdateEngine's own pre-#263 behavior when arraySeq was nil at
// construction.
type updateShutdownLookup struct {
	currentArray func() *job.ArraySequence
}

// Stop runs seq.StopForShutdown, never seq.Stop (#387): a
// Reboot is not a user asking the array to stay stopped once the box
// comes back up — persisting a new "stopped" state here would leave
// RestorePersistedMaintenance holding the array offline after the next
// ordinary boot. A persisted user `array stop` already in force is left
// exactly as it is.
func (u updateShutdownLookup) Stop(ctx context.Context) error {
	if u.currentArray == nil {
		return nil
	}
	seq := u.currentArray()
	if seq == nil {
		return nil
	}
	return seq.StopForShutdown(ctx)
}

// preUpdateBackup adapts *backup.Service to update.ConfigBackup, marking
// the pre-update archive with backup.ReasonPreUpdate (doc 10 §1, #401) so
// retention keeps it even if a later same-day backup — another update, or
// a config import — would otherwise take today's daily-tier slot and prune
// it. update.Engine's own ConfigBackup field only ever calls Run(ctx)
// error, so this is the narrowest way to pass the reason through without
// changing that interface.
type preUpdateBackup struct {
	svc *backup.Service
}

func (p preUpdateBackup) Run(ctx context.Context) error {
	return p.svc.RunReason(ctx, backup.ReasonPreUpdate)
}

// preTopologyBackup adapts *backup.Service to job.ConfigBackup, marking the
// archive with backup.ReasonPreTopology (doc 10 §1, #406) so retention
// keeps it the same way a pre-update or pre-import archive is kept. It is
// the type wireTopologyBackup wires into Scheduler.SetTopologyBackup, the
// one place a disk add/remove/replace/upgrade, format, or pool remount
// reaches this backup — none of them call backup.Service directly.
type preTopologyBackup struct {
	svc *backup.Service
}

func (p preTopologyBackup) Run(ctx context.Context) error {
	return p.svc.RunReason(ctx, backup.ReasonPreTopology)
}

// wireTopologyBackup connects backupService to scheduler through
// job.Scheduler.SetTopologyBackup, so a ClassTopology job runs the
// pre-topology backup the moment it actually starts — in Submit for one
// starting immediately, in dispatch() for one that waited its turn in the
// queue first (#406, #408). Kept as its own function, following
// wireBackup's own pattern above, so a test can call exactly what main.go
// calls rather than a hand copy of the assignment.
func wireTopologyBackup(scheduler *job.Scheduler, backupService *backup.Service) {
	scheduler.SetTopologyBackup(preTopologyBackup{svc: backupService})
}

func newUpdateEngine(ctx context.Context, cfg config, db *sql.DB, machineKey *auth.MachineKey, settings *api.SettingsService, scheduler *job.Scheduler, currentArray func() *job.ArraySequence, notifyService *notify.Service, runner disk.Runner, backupSvc *backup.Service) *update.Engine {
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/bin/hoservad"
	}
	current := packageVersion(ctx, runner, "hoserva")
	return &update.Engine{
		StateDir:    cfg.stateDir,
		SnapshotDir: filepath.Join(cfg.stateDir, "backups", "pre-migration"),
		DBPath:      filepath.Join(cfg.stateDir, "hoserva.db"),
		Current:     current,
		Fetcher:     update.HTTPFetcher{},
		Installer: update.SystemdInstaller{
			Runner:   runner,
			Hoservad: exe,
			StateDir: cfg.stateDir,
		},
		Host:     update.DebianHost{Runner: runner},
		Jobs:     scheduler,
		Shutdown: updateShutdownLookup{currentArray: currentArray},
		Backup:   preUpdateBackup{svc: backupSvc},
		Notify:   notifyService,
		Settings: api.NewUpdateSettings(settings),
	}
}
