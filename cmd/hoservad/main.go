// Command hoservad is the Hoserva daemon (doc 01 §4, §5, §7): it serves
// the API over a Unix socket (root/hoserva group, Q44) and a TLS-only TCP
// port (Q9, Q10), runs the job system, and serves the web UI for every
// non-API path.
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/cache"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
	"github.com/mdg-labs/hoserva/web"

	_ "modernc.org/sqlite"
)

// apiPathPrefix matches api/openapi.yaml's own /api/v1 versioning
// (doc 01 §5) — the one literal both listeners' mux registrations and
// apiv1.WithPathPrefix below share.
const apiPathPrefix = "/api/v1"

// logPruneInterval is how often the daily retention sweep runs (Q74):
// job logs and expired sessions, both under the state directory or in
// its database, never a data disk, so this is not the kind of timer
// CLAUDE.md forbids ("nothing on a timer walks a data disk").
const logPruneInterval = 24 * time.Hour

// notifyDeliveryInterval is how often the daemon drives
// notify.Service.RunDueDeliveries (#165) — the notify_deliveries queue
// lives in the central database, never a data disk, so this is not the
// kind of timer CLAUDE.md forbids. Short and frequent favors "retried and
// logged, never silently dropped" (#35's own acceptance criteria) over
// batching efficiency, which is the right tradeoff for a notification
// queue on a single-admin NAS.
const notifyDeliveryInterval = 30 * time.Second

// notifyDeliveryBatchLimit bounds how many due deliveries one
// RunDueDeliveries call attempts — comfortably above what a single-admin
// NAS's routing matrix can queue in one notifyDeliveryInterval tick.
const notifyDeliveryBatchLimit = 20

// notifyHTTPTimeout bounds every HTTP-based channel send (Gotify, ntfy,
// Discord, generic webhook) — without it, an unreachable or misconfigured
// channel's server could hang RunDueDeliveries indefinitely, since it
// processes deliveries one at a time and the daemon's own lifecycle
// context carries no deadline of its own.
const notifyHTTPTimeout = 30 * time.Second

// maxRequestBodyBytes bounds every unauthenticated and authenticated API
// request body alike (both listeners, cmd/hoservad's own
// http.MaxBytesHandler wrapping): every request this issue's API accepts
// is a small JSON object (login, TOTP enrolment/confirmation, first-admin
// creation), so 64 KiB is generous, not tight. Without this, an
// unauthenticated body of arbitrary size was read to completion —
// io.ReadAll, inside the generated decoder — before the rate limiter or
// any handler ever saw it: reproduced live, one 200 MiB unauthenticated
// POST to /auth/login raised the daemon's own VmHWM from 217 MB to 979
// MB, entirely from buffering a body nothing downstream needed more than
// a few dozen bytes of.
const maxRequestBodyBytes = 64 * 1024

// requestReadTimeout bounds how long net/http waits for a request's
// headers *and* body together (http.Server.ReadTimeout) — a second bound
// alongside maxRequestBodyBytes: the size cap stops an oversized body
// from ever being buffered, but a slow client trickling a body in just
// under that cap could otherwise still hold a connection (and the
// goroutine serving it) open indefinitely.
const requestReadTimeout = 15 * time.Second

type config struct {
	stateDir            string
	socketPath          string
	tcpAddr             string
	machineKeyPath      string
	configRoot          string
	allowAllSources     bool
	dev                 bool
	applyVerifiedUpdate string
	upsNotifyType       string
	upsNotifySet        bool
	upsShutdown         bool
}

func main() {
	cfg := parseFlags()
	switch {
	case cfg.applyVerifiedUpdate != "":
		if err := runApplyVerifiedUpdate(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "hoservad:", err)
			os.Exit(1)
		}
		return
	case cfg.upsNotifySet:
		if err := runUPSControlClient(cfg, cfg.upsNotifyType); err != nil {
			fmt.Fprintln(os.Stderr, "hoservad:", err)
			os.Exit(1)
		}
		return
	case cfg.upsShutdown:
		if err := runUPSControlClient(cfg, string(job.UPSNotifyLowBattery)); err != nil {
			fmt.Fprintln(os.Stderr, "hoservad:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "hoservad:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.stateDir, "state-dir", "/var/lib/hoserva", "state directory: database, job logs, migration snapshots")
	flag.StringVar(&cfg.socketPath, "socket-path", "/run/hoserva/hoserva.sock", "Unix socket path (Q44)")
	flag.StringVar(&cfg.tcpAddr, "tcp-addr", ":8008", "TLS-only TCP listen address (Q9)")
	flag.StringVar(&cfg.machineKeyPath, "machine-key-path", "/etc/hoserva/secret.key", "machine key path, encrypts secret columns (Q28)")
	flag.StringVar(&cfg.configRoot, "config-root", "/etc", "root for generated config files (doc 01 §2); disk mount units and snapraid.conf are written relative to this")
	flag.BoolVar(&cfg.allowAllSources, "allow-all-sources", false, "disable the LAN-only source filter (Q10) — WARNING: accepts connections from any address")
	flag.BoolVar(&cfg.dev, "dev", false, "development convenience: state dir, socket, machine key and TCP address default to a workspace-local path and 127.0.0.1 (git-ignored); an explicitly set flag always wins over this default")
	flag.StringVar(&cfg.applyVerifiedUpdate, "apply-verified-update", "", "install a verified pending .deb from this directory (transient unit; Q67)")
	flag.StringVar(&cfg.upsNotifyType, "ups-notify", "", "internal: relay a NUT upsmon NOTIFYTYPE (ONBATT/ONLINE/LOWBATT) to the running daemon's UPSController over its own control socket (doc 02 §6, Q77) — invoked by /usr/lib/hoserva/nut-notify, never by a user")
	flag.BoolVar(&cfg.upsShutdown, "ups-shutdown", false, "internal: run the running daemon's clean low-battery shutdown sequence synchronously over its control socket, exiting non-zero if any step failed — SHUTDOWNCMD's own entry point (doc 02 §6, Q77), invoked by /usr/lib/hoserva/nut-shutdown, never by a user")
	flag.Parse()

	// flag.Visit only visits flags actually given on the command line — so
	// --dev's own defaults below apply exactly to the flags the caller
	// didn't set themselves, never overriding one they did (an explicit
	// -tcp-addr, in particular, must win over --dev's own default, not
	// the reverse).
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	// An empty -ups-notify must still select client mode: falling through
	// to run() would open the live daemon's database and mark its running
	// jobs interrupted before the TCP listener fails (dialUPSControl
	// refuses the empty value).
	cfg.upsNotifySet = explicit["ups-notify"]

	if cfg.dev {
		if !explicit["state-dir"] {
			cfg.stateDir = "./.hoserva-dev"
		}
		if !explicit["socket-path"] {
			cfg.socketPath = "./.hoserva-dev/hoserva.sock"
		}
		if !explicit["machine-key-path"] {
			cfg.machineKeyPath = "./.hoserva-dev/secret.key"
		}
		if !explicit["config-root"] {
			cfg.configRoot = "./.hoserva-dev/etc"
		}
		if !explicit["tcp-addr"] {
			// The production default (":8008") binds every interface —
			// fine for a packaged install behind Q10's own source
			// filter, but a dev run has no such expectation and
			// shouldn't listen on the LAN just because --dev was passed.
			cfg.tcpAddr = "127.0.0.1:8008"
		}
	}
	if persisted, ok := loadPersistedHTTPS(cfg.stateDir); ok {
		if !explicit["allow-all-sources"] {
			cfg.allowAllSources = persisted.AllowAllSources
		}
		if !explicit["tcp-addr"] && persisted.ListenPort > 0 {
			cfg.tcpAddr = applyPersistedListenPort(cfg.tcpAddr, persisted.ListenPort)
		}
	}
	return cfg
}

func run(cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	db, err := openDatabase(cfg.stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := applyMigrations(ctx, db, cfg.stateDir); err != nil {
		return err
	}

	authStore := api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, cfg.machineKeyPath, authStore)
	if err != nil {
		return fmt.Errorf("loading machine key: %w", err)
	}

	authService := api.NewAuthService(authStore, machineKey)

	notifyStore := notify.NewStore(db)
	notifyHub := notify.NewHub()
	notifyService := notify.NewService(notifyStore, machineKey, notify.DefaultSenders(&http.Client{Timeout: notifyHTTPTimeout}))
	notifyService.Hub = notifyHub

	settingsStore := api.NewSettingsStore(db)
	settingsService := api.NewSettingsService(settingsStore, machineKey)
	scheduleService := api.NewScheduleService(api.NewScheduleStore(db), settingsStore)
	upsStore := api.NewUPSStore(db)

	logsDir := filepath.Join(cfg.stateDir, "jobs")
	jobStore := job.NewStore(db)
	logs := job.NewLogStore(logsDir)
	hub := job.NewHub()
	registry := job.NewRegistry()
	linuxDisks := disk.NewLinuxProvider()
	history := store.NewHistory(db)
	disks := newPersistingDiskProvider(linuxDisks, history)
	arrayStore := store.NewArrayStore(db)
	shareStore := store.NewShareStore(db)
	configRoot := cfg.configRoot
	if configRoot == "" {
		configRoot = "/etc"
	}
	generator := cfggen.NewGenerator(configRoot)
	// The mover cooperatively checks StopRequested between files and
	// leaves consistent on-disk state at any stopping point (a duplicate,
	// never a gap — doc 09 §2), so it honestly supports being cancelled,
	// same as TypeACMEIssue below.
	moverResults := cache.NewResultStore(db)
	registry.Register(job.TypeMover, true, job.RunMover(job.MoverDeps{
		Shares:  moverSharesFromStore(shareStore, arrayStore),
		Results: moverResults,
		UsagePlan: func(ctx context.Context) (string, []cache.UsageShare, error) {
			_, disks, err := arrayStore.GetArray(ctx)
			if err != nil {
				if errors.Is(err, store.ErrNoArray) {
					return "", nil, nil
				}
				return "", nil, fmt.Errorf("loading array topology for cache usage: %w", err)
			}
			var cacheMount string
			for _, d := range disks {
				if d.Role == store.ArrayRoleCache {
					cacheMount = d.Mountpoint
					break
				}
			}
			if cacheMount == "" {
				return "", nil, nil
			}
			all, err := shareStore.List(ctx)
			if err != nil {
				return "", nil, fmt.Errorf("listing shares for cache usage: %w", err)
			}
			out := make([]cache.UsageShare, 0, len(all))
			for _, s := range all {
				out = append(out, cache.UsageShare{
					Name: s.Name,
					Path: cacheMount + "/" + s.Name,
					Mode: s.CacheMode,
				})
			}
			return cacheMount, out, nil
		},
	}))
	scheduler := job.NewScheduler(jobStore, logs, hub, registry)
	if err := scheduler.RecoverFromRestart(ctx); err != nil {
		return fmt.Errorf("recovering jobs after restart: %w", err)
	}

	arraySeq, err := newArraySequence(ctx, scheduler, arrayStore, shareStore, disks, linuxDisks.Exec)
	if err != nil {
		return fmt.Errorf("building array stop/start sequence: %w", err)
	}
	// handler is created here, ahead of its other fields, so upsController
	// and updateEngine (below) can both resolve handler.CurrentArray at
	// shutdown time instead of capturing arraySeq's own startup value
	// (#263) — the ArrayReady hook (further down) is this value's only
	// writer once the daemon starts serving requests.
	handler := &api.Handler{}
	handler.SetArray(arraySeq)
	upsController := newUPSController(scheduler, handler.CurrentArray, notifyService, linuxDisks.Exec)

	// Losing metrics.db must not look like array failure (#186): log and
	// leave Handler.Metrics nil so GET /metrics returns an empty series.
	metricsStore, err := metrics.Open(ctx, filepath.Join(cfg.stateDir, "metrics.db"))
	if err != nil {
		log.Printf("hoservad: opening metrics database: %v — GET /metrics will return empty series", err)
		metricsStore = nil
	} else {
		defer func() { _ = metricsStore.Close() }()
	}

	parityEngine, err := newSnapraidEngine(configRoot, cfg.stateDir, nil)
	if err != nil {
		return fmt.Errorf("opening snapraid.conf: %w", err)
	}
	chainGuard := &diffGuardHolder{}
	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   cfg.stateDir,
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shareStore,
		arrayStore: arrayStore,
		chainGuard: chainGuard,
		generator:  generator,
		diskUnits:  disk.SystemdMounter{Runner: linuxDisks.Exec},
		mounts:     disk.KernelMounts{Runner: linuxDisks.Exec},
	}
	if parityEngine != nil {
		parityReg.register(parityEngine)
	}
	backupService := newBackupService(ctx, cfg, db, machineKey, settingsService, linuxDisks.Exec)
	acmeStore := acme.NewStore(db)
	acmeService := &acme.Service{
		Store:     acmeStore,
		Cipher:    machineKey,
		Client:    &acme.ProductionClient{},
		Publisher: &acmeNotify{svc: notifyService},
	}
	backupService.Secrets = &backup.ServiceSecretSource{
		BackupPassphraseFn: settingsService.BackupPassphrase,
		DatabaseSecretsFn: func(reqCtx context.Context) ([]backup.DatabaseSecret, error) {
			acmeSecrets, err := acmeDatabaseSecrets(reqCtx, acmeStore)
			if err != nil {
				return nil, err
			}
			upsSecrets, err := upsDatabaseSecrets(reqCtx, upsStore)
			if err != nil {
				return nil, err
			}
			return append(acmeSecrets, upsSecrets...), nil
		},
	}
	updateEngine := newUpdateEngine(ctx, cfg, db, machineKey, settingsService, scheduler, handler.CurrentArray, notifyService, linuxDisks.Exec, backupService)
	networkSvc := &cfggen.NetworkService{
		Generator: generator,
		Detector:  cfggen.ExecDetector{Root: configRoot},
		Runner:    rootedIfupdown{root: configRoot, inner: cfggen.ExecIfupdown{}},
		Links:     cfggen.LinuxLinks{},
		StateDir:  cfg.stateDir,
	}
	if err := networkSvc.Recover(ctx); err != nil {
		log.Printf("hoservad: restoring unconfirmed network change: %v", err)
	}
	var shareUsages share.UsageReader
	if parityEngine != nil {
		shareUsages = parityEngine.Usage
	}
	// rebuildArraySequence is also the ArrayReady hook job.TypeDiskFormat/
	// DiskAdd/DiskReplace call below, moved up here (from its previous
	// position right after this point) so shareService can already close
	// over it as its PostCommit: a live createShare/updateShare/
	// deleteShare changes store.ShareStore directly, never through one of
	// those disk-topology jobs, so without this same rebuild running
	// after every one of those calls too, Handler.Array's own ShareMounts
	// stays exactly as stale as it was at the last daemon start or
	// disk-topology change — array/stop then fails EBUSY on a share that
	// exists and is mounted, but that the running daemon has never once
	// rebuilt its ArraySequence to know about (#268).
	rebuildArraySequence := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrayStore, shareStore, disks, linuxDisks.Exec)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		return nil
	}
	shareService := newShareService(shareStore, arrayStore, generator, pool.SystemdMounter{Runner: linuxDisks.Exec}, shareUsages)
	shareService.PostCommit = rebuildArraySequence
	// topologyChanged is the disk-topology jobs' ArrayReady hook, built by
	// wireTopologyHooks below so the lab tests (parity_registrar_lab_test.go,
	// #265; evacuation_removal_state_lab_test.go, #359) build the identical
	// hooks from the same function rather than reimplementing them.
	topologyChanged := wireTopologyHooks(shareService, rebuildArraySequence, parityReg, handler)
	handler.Scheduler = scheduler
	handler.Store = jobStore
	handler.Logs = logs
	handler.Auth = authService
	handler.Notify = notifyService
	handler.Settings = settingsService
	handler.Schedules = scheduleService
	handler.UPS = api.NewUPSService(upsStore, machineKey, generator, newNUTReloader(linuxDisks.Exec), upsSocketPermissions{path: upsControlSocketPath(cfg.socketPath)})
	handler.Disks = disks
	handler.Metrics = metricsStore
	handler.History = history
	handler.Updates = updateEngine
	handler.Generator = generator
	handler.HostConfig = store.NewHostConfigStore(db)
	handler.Docker = cfggen.ExecDocker{}
	handler.ArrayStore = arrayStore
	handler.Network = networkSvc
	handler.ACME = acmeService
	handler.Shares = shareService
	handler.MoverResults = moverResults

	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:   disks,
		Runner:     linuxDisks.Exec,
		Store:      arrayStore,
		Generator:  generator,
		Mounter:    disk.SystemdMounter{Runner: linuxDisks.Exec},
		ArrayReady: topologyChanged,
	}))
	registry.Register(job.TypeDiskAdd, false, job.RunDiskAdd(job.DiskAddDeps{
		Provider:   disks,
		Runner:     linuxDisks.Exec,
		Store:      arrayStore,
		Generator:  generator,
		Mounter:    disk.SystemdMounter{Runner: linuxDisks.Exec},
		ArrayReady: topologyChanged,
	}))
	// replaceParityEngine is left a true nil interface, not a non-nil
	// interface wrapping a nil *parity.SnapraidEngine, when snapraid.conf
	// doesn't exist yet (no array created): RunDiskReplace's own
	// dependency check (d.Parity == nil) only catches the former, and its
	// GetArray call already fails closed with ErrNoArray in that case
	// before ever reaching Parity — this is only extra safety against
	// arrayStore and snapraid.conf ever disagreeing about whether an
	// array exists.
	var replaceParityEngine parity.Engine
	if parityEngine != nil {
		replaceParityEngine = parityEngine
	}
	// TypeDiskReplace's own snapraid fix step genuinely honors context
	// cancellation (exec.CommandContext kills the subprocess, #288's own
	// lab test proves this), so it is registered cancellable — a stuck or
	// unwanted replace can be cancelled the same way a mover or ACME issue
	// job already can, leaving the array's topology already switched over
	// to the replacement and recoverable via an ordinary `hoserva fix`.
	registry.Register(job.TypeDiskReplace, true, job.RunDiskReplace(job.DiskReplaceDeps{
		Provider:   disks,
		Runner:     linuxDisks.Exec,
		Store:      arrayStore,
		Generator:  generator,
		Mounter:    disk.SystemdMounter{Runner: linuxDisks.Exec},
		Parity:     replaceParityEngine,
		ArrayReady: topologyChanged,
	}))
	// The data-disk upgrade (doc 02 §4 state machine): its run, its abort
	// (Cancel of a queued or interrupted upgrade) and startup recovery
	// share one set of dependencies, so all three unwind the same way.
	// Registered cancellable: a cancel before the release decision is the
	// abort, and the scheduler refuses it from releasing on.
	upgradeDataDeps := job.DiskUpgradeDataDeps{
		Provider:   disks,
		Runner:     linuxDisks.Exec,
		Store:      arrayStore,
		Generator:  generator,
		Parity:     replaceParityEngine,
		Mounts:     disk.KernelMounts{Runner: linuxDisks.Exec},
		Array:      handler.CurrentArray,
		ArrayReady: topologyChanged,
	}
	registry.Register(job.TypeDiskUpgradeData, true, job.RunDiskUpgradeData(upgradeDataDeps))
	registry.RegisterAbort(job.TypeDiskUpgradeData, job.AbortDiskUpgradeData(upgradeDataDeps))
	registry.Register(job.TypeDiskUpgradeParity, true, job.RunDiskUpgradeParity(job.DiskUpgradeParityDeps{
		Provider:       disks,
		Runner:         linuxDisks.Exec,
		Store:          arrayStore,
		Generator:      generator,
		Mounter:        disk.SystemdMounter{Runner: linuxDisks.Exec},
		UpgradeMounter: disk.DirectMounter{Runner: linuxDisks.Exec},
		Parity:         replaceParityEngine,
		ArrayReady:     topologyChanged,
	}))

	// Startup recovery (doc 02 §4 E4, UR1, UR8) runs before any listener
	// or loop below can submit or resume a job. A failure is recorded on
	// the job, and the daemon still starts so resume and cancel stay
	// reachable.
	if err := job.RecoverDiskUpgradeData(ctx, scheduler, upgradeDataDeps); err != nil {
		log.Printf("hoservad: data-disk upgrade startup recovery: %v", err)
	}

	webRoot, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return fmt.Errorf("opening embedded web assets: %w", err)
	}

	tcpServer, err := buildTCPServer(handler, authStore, authService, hub, notifyHub, webRoot)
	if err != nil {
		return fmt.Errorf("building TCP server: %w", err)
	}
	unixServer, err := buildUnixServer(handler, authStore, hub, notifyHub)
	if err != nil {
		return fmt.Errorf("building Unix socket server: %w", err)
	}

	tcpListener, httpsCtrl, err := buildTCPListener(cfg)
	if err != nil {
		return fmt.Errorf("starting TCP listener: %w", err)
	}
	handler.HTTPS = httpsCtrl
	acmeService.Installer = httpsCtrl
	registry.Register(job.TypeACMEIssue, true, job.RunACMEIssue(acmeService.Issue))
	unixListener, err := setupUnixListener(cfg.socketPath)
	if err != nil {
		return fmt.Errorf("starting Unix socket listener: %w", err)
	}
	applySocketGroupPermissions(cfg.socketPath, hoservaGroup)
	upsControlListener, err := setupUnixListener(upsControlSocketPath(cfg.socketPath))
	if err != nil {
		return fmt.Errorf("starting ups control socket listener: %w", err)
	}
	applySocketGroupPermissions(upsControlSocketPath(cfg.socketPath), cfggen.NUTGroup)
	go serveUPSControl(ctx, upsControlListener, upsController, auth.OSGroupLookup{}, uint32(os.Getuid()), cfggen.NUTGroup)

	pruneOnce(ctx, jobStore, logs, authStore, history)
	go runDailyPrune(ctx, jobStore, logs, authStore, history)
	go runNotifyDeliveryLoop(ctx, notifyService, notifyDeliveryInterval, notifyDeliveryBatchLimit)
	go runMoverThresholdLoop(ctx, &moverThresholdRunner{
		Scheduler: scheduler,
		Jobs:      jobStore,
		Array:     arrayStore,
		Statter:   pool.StatfsSpaceStatter{},
	}, moverThresholdInterval)
	go runSpaceAlertLoop(ctx, &spaceAlertRunner{
		Array:    arrayStore,
		Statter:  pool.StatfsSpaceStatter{},
		Notifier: notifyService,
	}, spaceAlertInterval)
	go runScheduleLoop(ctx, &scheduleRunner{
		Schedules: scheduleService,
		Scheduler: scheduler,
		Guard:     chainGuard,
		Backup:    backupService,
		Notifier:  &scheduleNotifier{svc: notifyService},
		ACME:      acmeService,
		Jobs:      jobStore,
	}, scheduleTickInterval)

	errCh := make(chan error, 2)
	go func() {
		log.Printf("hoservad: serving the API and web UI over TLS on %s", cfg.tcpAddr)
		errCh <- tcpServer.Serve(tcpListener)
	}()
	go func() {
		log.Printf("hoservad: serving the API over the Unix socket at %s", cfg.socketPath)
		errCh <- unixServer.Serve(unixListener)
	}()

	// A listener that stops on its own (bind lost, fd exhaustion, ...) is
	// reported as a genuine failure — never silently swallowed to a clean
	// exit 0, which would leave systemd's Restart=on-failure with nothing
	// to react to.
	var runErr error
	select {
	case <-ctx.Done():
		log.Println("hoservad: shutting down")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("hoservad: a listener stopped unexpectedly: %v", err)
			runErr = fmt.Errorf("a listener stopped unexpectedly: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = tcpServer.Shutdown(shutdownCtx)
	_ = unixServer.Shutdown(shutdownCtx)
	_ = upsControlListener.Close()
	networkSvc.Close()
	return runErr
}

// liveUpdateFailure is what a topology hook does when the running pool
// does not take the regenerated mounts.
type liveUpdateFailure int

const (
	// logLiveUpdateFailure logs it and succeeds: the disk-topology jobs
	// cannot be retried once the disk is a member, and the next array
	// start applies the same mounts.
	logLiveUpdateFailure liveUpdateFailure = iota
	// failOnLiveUpdateFailure returns it: an evacuation must not copy
	// anything while its disk still takes new writes in a live mount
	// (doc 09 §4 step 2, #359), and a disk removal must not take a disk
	// out of SnapRAID while it is still a branch of a live mount (step 7,
	// #358).
	failOnLiveUpdateFailure
)

// wireTopologyHooks builds both topology hooks run() uses and returns
// topologyChanged, the disk-topology jobs' ArrayReady hook, which only
// logs a failed live update. It binds parityReg's arrayReady — the hook
// job.TypeEvacuation's run and abort and job.TypeDiskRemove call — to the
// variant that returns that failure instead, before either can run.
func wireTopologyHooks(shareService *share.Service, rebuildArraySequence func(ctx context.Context) error, parityReg *parityRegistrar, handler *api.Handler) func(ctx context.Context) error {
	parityReg.arrayReady = newTopologyChangedHook(shareService, rebuildArraySequence, parityReg, handler, failOnLiveUpdateFailure)
	return newTopologyChangedHook(shareService, rebuildArraySequence, parityReg, handler, logLiveUpdateFailure)
}

// newTopologyChangedHook builds an ArrayReady hook: shareService.
// ApplyTopology rewrites every pool mount unit and share file from the
// store (the disk-topology jobs themselves write only the catch-all's
// unit); then rebuildArraySequence rebuilds Handler's ArraySequence from
// the same store; then parityReg.ensure notices when a live `POST
// /disks/array` has just written snapraid.conf where nothing existed at
// startup and wires TypeSync/TypeScrub/TypeFix/TypeShareRelocation/
// TypeRebalance/TypeEvacuation and Handler's own parity-derived fields
// the same way startup does, so none of them need a restart (#265) — a
// no-op once parity is already wired. Last, when the catch-all is
// mounted, the rebuilt sequence is applied to the running pool, so an
// added disk's capacity is available at once (doc 02 §4 "Adding a disk"
// step 6) and a disk in removal is no-create at once (doc 09 §4 step 2).
// onLiveFailure decides whether a failure of that last step is logged or
// returned.
func newTopologyChangedHook(shareService *share.Service, rebuildArraySequence func(ctx context.Context) error, parityReg *parityRegistrar, handler *api.Handler, onLiveFailure liveUpdateFailure) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		live := pool.IsMounted(pool.CatchAllPath)
		if err := shareService.ApplyTopology(ctx, live); err != nil {
			return fmt.Errorf("regenerating share configuration: %w", err)
		}
		if err := rebuildArraySequence(ctx); err != nil {
			return err
		}
		if err := parityReg.ensure(ctx); err != nil {
			return fmt.Errorf("wiring the parity engine: %w", err)
		}
		if seq := handler.CurrentArray(); seq != nil {
			if err := seq.RefreshLive(ctx, live); err != nil {
				if onLiveFailure == failOnLiveUpdateFailure {
					return fmt.Errorf("applying the new pool mounts to the running pool: %w", err)
				}
				log.Printf("hoservad: the running pool did not pick up the new disk topology: %v — stop and start the array to apply it", err)
			}
		}
		return nil
	}
}

// parityRegistrar wires every parity-dependent job type
// (TypeSync/TypeScrub/TypeFix/TypeShareRelocation/TypeRebalance/
// TypeEvacuation/TypeDiskRemove) and Handler's own parity-derived fields
// (Handler.SetParity) against a *parity.SnapraidEngine, exactly once: at
// startup, when an array already exists, or — since #265 — the first
// time topologyChanged's ArrayReady hook observes that a live `POST
// /disks/array` has just written snapraid.conf where nothing existed
// before. job.Registry.Register panics on a second registration for the
// same type (its own doc comment), so ensure's mu serializes it against a
// concurrent ArrayReady call trying to run it a second time, the same
// concurrency shape Handler.SetArray/CurrentArray already established for
// Handler.Array (#263).
type parityRegistrar struct {
	mu         sync.Mutex
	done       bool
	configRoot string
	stateDir   string
	db         *sql.DB
	registry   *job.Registry
	handler    *api.Handler
	shareStore *store.ShareStore
	arrayStore *store.ArrayStore
	chainGuard *diffGuardHolder
	// arrayReady is the topology hook that returns a failed live update
	// (wireTopologyHooks), set after both it and p are constructed —
	// register below only captures p.callArrayReady, a method value that
	// reads this field at call time, since register can run (at startup,
	// when an array already exists) before the hook exists at all (#359).
	arrayReady func(ctx context.Context) error
	// generator, diskUnits and mounts are TypeDiskRemove's own: the
	// config generator every array file is written through, what stops
	// a data disk's own mount unit (disk.SystemdMounter), and the kernel
	// mount table (disk.KernelMounts).
	generator *cfggen.Generator
	diskUnits disk.UnitMounter
	mounts    job.MountTable
}

// shareNamesFromStore lists every share's name: TypeDiskRemove's
// post-check looks at each one's branch on the disk being removed.
func shareNamesFromStore(shares *store.ShareStore) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		rows, err := shares.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, r.Name)
		}
		return names, nil
	}
}

// callArrayReady is job.EvacuationDeps.ArrayReady/EvacuationAbort's and
// job.DiskRemoveDeps.ArrayReady's own dependency: a stable method value
// register can capture before p.arrayReady is wired, since none of them
// ever runs before startup finishes wiring it (this type's own field doc
// comment).
func (p *parityRegistrar) callArrayReady(ctx context.Context) error {
	if p.arrayReady == nil {
		return fmt.Errorf("hoservad: the array-ready hook is not wired yet")
	}
	return p.arrayReady(ctx)
}

// register wires engine's job types and Handler's own parity-derived
// fields and marks p done. Callers must already know engine is non-nil
// and that registration has not happened yet — main's own startup call
// runs before any listener or ArrayReady hook can race it, and ensure
// below is what makes both true for every later call.
func (p *parityRegistrar) register(engine *parity.SnapraidEngine) {
	engine.Usage = parity.NewUsageStore(p.db)
	engine.Relocation = parity.NewRelocationManifestStore(p.db)

	p.registry.Register(job.TypeSync, false, job.RunSync(engine))
	p.registry.Register(job.TypeScrub, false, job.RunScrub(engine))
	p.registry.Register(job.TypeFix, false, job.RunFix(engine))
	p.registry.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
		Share:    shareRelocationShareFromStore(p.shareStore, p.arrayStore),
		Sync:     shareRelocationSyncFunc(engine),
		Manifest: engine.Relocation,
	}))
	rebalanceShares := rebalanceSharesFromStore(p.shareStore, p.arrayStore)
	rebalanceTracked := rebalanceTrackedFileCount(engine)
	p.registry.Register(job.TypeRebalance, true, job.RunRebalance(job.RebalanceDeps{
		Sync:             shareRelocationSyncFunc(engine),
		TrackedFileCount: rebalanceTracked,
		Store:            p.arrayStore,
	}))
	p.registry.Register(job.TypeEvacuation, true, job.RunEvacuation(job.EvacuationDeps{
		Sync:             evacuationSyncFunc(engine),
		TrackedFileCount: rebalanceTracked,
		Shares:           rebalanceShares,
		Manifest:         engine.Relocation,
		Store:            p.arrayStore,
		ArrayReady:       p.callArrayReady,
	}))
	// Cancelling an evacuation that is already StatusInterrupted never
	// re-enters RunEvacuation, so it needs its own path to clear a
	// stale removing-disks exemption and removal state (job.EvacuationAbort's
	// own doc comment).
	p.registry.RegisterAbort(job.TypeEvacuation, job.EvacuationAbort(engine.Relocation, p.arrayStore, p.callArrayReady))
	// Finishing a disk's removal (doc 09 §4 steps 7-9, #358). Not
	// cancellable: each step is short and recorded in the disk's removal
	// state, and a failed or interrupted run is finished by running it
	// again, never undone half-way.
	p.registry.Register(job.TypeDiskRemove, false, job.RunDiskRemove(job.DiskRemoveDeps{
		Store:      p.arrayStore,
		Generator:  p.generator,
		Mounts:     p.mounts,
		Unmounter:  p.diskUnits,
		Parity:     engine,
		ShareNames: shareNamesFromStore(p.shareStore),
		ArrayReady: p.callArrayReady,
	}))

	p.handler.SetParity(engine, engine.Guard, engine.Relocation, rebalanceShares)
	p.chainGuard.set(job.EngineDiffGuard{Engine: engine, Guard: engine.Guard})
	p.done = true
}

// ensure is topologyChanged's own ArrayReady hook: a no-op once parity is
// already wired (whether that happened at startup or from an earlier
// live array creation), and otherwise opens snapraid.conf fresh — the
// live CreateArray job's own Generator has already written it by the
// time ArrayReady runs — and calls register on what it finds.
func (p *parityRegistrar) ensure(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return nil
	}
	engine, err := newSnapraidEngine(p.configRoot, p.stateDir, nil)
	if err != nil {
		return fmt.Errorf("opening snapraid.conf: %w", err)
	}
	if engine == nil {
		return nil
	}
	p.register(engine)
	return nil
}

func openDatabase(stateDir string) (*sql.DB, error) {
	dbPath := filepath.Join(stateDir, "hoserva.db")
	if err := ensureDatabaseFileMode(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening database at %s: %w", dbPath, err)
	}
	return db, nil
}

// ensureDatabaseFileMode creates the database file at 0600 the first
// time it's opened, and re-tightens it on every later start — the state
// directory's own 0700 mode already keeps other accounts out, but the
// database file itself holds every encrypted secret column (Q28) and
// should not rely on that alone (mirroring the machine key and TLS key's
// own explicit 0600).
func ensureDatabaseFileMode(path string) error {
	if _, err := os.Stat(path); err == nil {
		return os.Chmod(path, 0o600)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat database file at %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("creating database file at %s: %w", path, err)
	}
	return f.Close()
}

func applyMigrations(ctx context.Context, db *sql.DB, stateDir string) error {
	migrations, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}
	snapshotDir := filepath.Join(stateDir, "backups", "pre-migration")
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: snapshotDir}
	applied, snapshotPath, err := runner.Apply(ctx)
	if err != nil {
		return fmt.Errorf("applying schema migrations: %w", err)
	}
	if len(applied) > 0 {
		log.Printf("hoservad: applied %d schema migration(s), snapshot at %s", len(applied), snapshotPath)
	}
	return nil
}

// withSourceAddrMiddleware attaches the request's source address to its
// context (stripped of port) so Login can rate-limit per source address
// (doc 01 §7) — the generated Handler interface never hands a method the
// raw *http.Request, only ctx and typed parameters (D18).
func withSourceAddrMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		}
		next.ServeHTTP(w, r.WithContext(api.WithSourceAddr(r.Context(), host)))
	})
}

func buildTCPServer(handler *api.Handler, authStore *api.AuthStore, authService *api.AuthService, hub *job.Hub, notifyHub *notify.Hub, webRoot fs.FS) (*http.Server, error) {
	security := &api.SessionSecurityHandler{Auth: authService}
	apiServer, err := apiv1.NewServer(handler, security,
		apiv1.WithPathPrefix(apiPathPrefix),
		apiv1.WithNotFound(jsonAPINotFoundHandler),
		apiv1.WithErrorHandler(api.WriteDecodeError),
	)
	if err != nil {
		return nil, fmt.Errorf("building generated API server: %w", err)
	}

	events := &api.EventsHandler{Hub: hub, NotifyHub: notifyHub, Authenticate: tcpEventsAuthenticate(authService)}

	mux := http.NewServeMux()
	// http.MaxBytesHandler wraps both API routes, not the SPA branch
	// below (a GET with no body has nothing for it to bound) — see
	// maxRequestBodyBytes's own doc comment.
	mux.Handle(apiPathPrefix+"/events", http.MaxBytesHandler(api.SetupGate(withSourceAddrMiddleware(events), authStore, apiPathPrefix), maxRequestBodyBytes))
	mux.Handle(apiPathPrefix+"/", http.MaxBytesHandler(api.SetupGate(withSourceAddrMiddleware(apiServer), authStore, apiPathPrefix), maxRequestBodyBytes))
	mountAPINotFoundRoutes(mux)
	mux.Handle("/", spaHandler(webRoot))

	return &http.Server{
		Handler: mux,
		// Bounds the TLS handshake itself too (net/http applies
		// ReadHeaderTimeout as the connection's read deadline before
		// ever calling tls.Conn.Handshake) — closing the other half of
		// the idle-connection review finding this issue fixes: without
		// it, a client that sends the ClientHello's first byte and then
		// goes silent holds a goroutine (and, before the sniffing
		// listener's own async fix, every other pending Accept) forever.
		ReadHeaderTimeout: 10 * time.Second,
		// Bounds the body read that follows the headers too — a slow
		// client trickling a body in under maxRequestBodyBytes could
		// otherwise still hold the connection open indefinitely.
		ReadTimeout: requestReadTimeout,
		IdleTimeout: 120 * time.Second,
	}, nil
}

// tcpEventsAuthenticate builds EventsHandler's Authenticate seam for the
// TCP transport: a real session cookie, checked the same way
// SessionSecurityHandler validates one for every other operation — viewer
// is /api/v1/events's own x-hoserva-role, satisfied by any signed-in
// user.
func tcpEventsAuthenticate(authService *api.AuthService) func(r *http.Request) error {
	return func(r *http.Request) error {
		cookie, err := r.Cookie("hoserva_session")
		if err != nil {
			return err
		}
		_, err = authService.ValidateSession(r.Context(), cookie.Value)
		return err
	}
}

func buildUnixServer(handler *api.Handler, authStore *api.AuthStore, hub *job.Hub, notifyHub *notify.Hub) (*http.Server, error) {
	security := api.TrustedSecurityHandler{}
	apiServer, err := apiv1.NewServer(handler, security,
		apiv1.WithPathPrefix(apiPathPrefix),
		apiv1.WithNotFound(jsonAPINotFoundHandler),
		apiv1.WithErrorHandler(api.WriteDecodeError),
	)
	if err != nil {
		return nil, fmt.Errorf("building generated API server: %w", err)
	}

	// The connection's own SO_PEERCRED already authorized it before any
	// request on it reaches this handler at all (unixSocketAuthMiddleware,
	// below) — there is nothing left to authenticate for /events.
	events := &api.EventsHandler{Hub: hub, NotifyHub: notifyHub, Authenticate: func(r *http.Request) error { return nil }}

	mux := http.NewServeMux()
	// http.MaxBytesHandler wraps both API routes — see
	// maxRequestBodyBytes's own doc comment; the Unix socket is
	// peer-credential-trusted, not unauthenticated, but a local caller
	// sending an oversized body by mistake shouldn't cost the daemon
	// unbounded memory either.
	mux.Handle(apiPathPrefix+"/events", http.MaxBytesHandler(api.SetupGate(events, authStore, apiPathPrefix), maxRequestBodyBytes))
	mux.Handle(apiPathPrefix+"/", http.MaxBytesHandler(api.SetupGate(apiServer, authStore, apiPathPrefix), maxRequestBodyBytes))
	mountAPINotFoundRoutes(mux)

	guarded := unixSocketAuthMiddleware(mux, auth.OSGroupLookup{}, uint32(os.Getuid()))
	return &http.Server{
		Handler:           guarded,
		ConnContext:       unixConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       requestReadTimeout,
		IdleTimeout:       120 * time.Second,
	}, nil
}

func buildTCPListener(cfg config) (net.Listener, *httpsControl, error) {
	ln, err := net.Listen("tcp", cfg.tcpAddr)
	if err != nil {
		return nil, nil, err
	}
	filtered := newSourceFilteringListener(ln, cfg.allowAllSources)

	certPath := filepath.Join(cfg.stateDir, "tls", "hoserva.crt")
	keyPath := filepath.Join(cfg.stateDir, "tls", "hoserva.key")
	cert, err := loadOrGenerateTLSCertificate(certPath, keyPath)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	httpsCtrl := newHTTPSControl(cfg.stateDir, certPath, keyPath, filtered, listenPortOf(cfg.tcpAddr), cfg.allowAllSources, cert)
	tlsConfig := &tls.Config{GetCertificate: httpsCtrl.GetCertificate, MinVersion: tls.VersionTLS12}
	return newTLSSniffingListener(filtered, tlsConfig), httpsCtrl, nil
}

// pruneOnce runs every retention sweep this daemon does on its own state
// (never a data disk — CLAUDE.md's "nothing on a timer walks a data
// disk" doesn't apply to any of this): job logs (Q74), spin-state events
// and audit-log rows, and expired sessions, which otherwise accumulate
// in the central database forever since nothing else ever deletes them
// past their own retention.
func pruneOnce(ctx context.Context, jobStore *job.Store, logs *job.LogStore, authStore *api.AuthStore, history *store.History) {
	active, err := activeJobIDs(ctx, jobStore)
	if err != nil {
		log.Printf("hoservad: listing active jobs for log retention: %v", err)
	} else if err := logs.Prune(time.Now(), active); err != nil {
		log.Printf("hoservad: pruning job logs: %v", err)
	}

	if err := authStore.DeleteExpiredSessions(ctx, time.Now()); err != nil {
		log.Printf("hoservad: pruning expired sessions: %v", err)
	}

	if err := history.PruneHistory(ctx, time.Now()); err != nil {
		log.Printf("hoservad: pruning spin-state events and audit log: %v", err)
	}
}

func runDailyPrune(ctx context.Context, jobStore *job.Store, logs *job.LogStore, authStore *api.AuthStore, history *store.History) {
	ticker := time.NewTicker(logPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneOnce(ctx, jobStore, logs, authStore, history)
		}
	}
}

// runNotifyDeliveryLoop drives notify.Service.RunDueDeliveries (#165) on
// interval, attempting up to limit due deliveries per tick, until ctx is
// cancelled — the same shutdown discipline as runDailyPrune. A failure
// here is the delivery store itself misbehaving, not a channel-side send
// failure (RunDueDeliveries already handles and persists those without
// returning an error), so it is logged and the loop keeps ticking rather
// than exiting.
func runNotifyDeliveryLoop(ctx context.Context, notifyService *notify.Service, interval time.Duration, limit int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := notifyService.RunDueDeliveries(ctx, limit); err != nil {
				log.Printf("hoservad: running due notification deliveries: %v", err)
			}
		}
	}
}

func activeJobIDs(ctx context.Context, jobStore *job.Store) (map[string]bool, error) {
	active, err := jobStore.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(active))
	for _, j := range active {
		ids[j.ID] = true
	}
	return ids, nil
}
