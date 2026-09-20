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

func newBackupService(ctx context.Context, cfg config, db *sql.DB, machineKey *auth.MachineKey, settings *api.SettingsService, runner disk.Runner) *backup.Service {
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
		Secrets: &backup.ServiceSecretSource{BackupPassphraseFn: settings.BackupPassphrase},
		Cipher:  machineKey,
		Version: packageVersion(ctx, runner, "hoserva"),
	}
}

func newUpdateEngine(ctx context.Context, cfg config, db *sql.DB, machineKey *auth.MachineKey, settings *api.SettingsService, scheduler *job.Scheduler, arraySeq *job.ArraySequence, notifyService *notify.Service, runner disk.Runner, backupSvc *backup.Service) *update.Engine {
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/bin/hoservad"
	}
	current := packageVersion(ctx, runner, "hoserva")
	var shutdown update.Shutdown
	if arraySeq != nil {
		shutdown = arraySeq
	}
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
		Shutdown: shutdown,
		Backup:   backupSvc,
		Notify:   notifyService,
		Settings: api.NewUpdateSettings(settings),
	}
}
