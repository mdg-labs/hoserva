// Command hoservad is the Hoserva daemon (doc 01 §4, §5, §7): it serves
// the API over a Unix socket (root/hoserva group, Q44) and a TLS-only TCP
// port (Q9, Q10), runs the job system, and serves the web UI for every
// non-API path.
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
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
	stateDir        string
	socketPath      string
	tcpAddr         string
	machineKeyPath  string
	configRoot      string
	allowAllSources bool
	dev             bool
}

func main() {
	cfg := parseFlags()
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
	flag.Parse()

	// flag.Visit only visits flags actually given on the command line — so
	// --dev's own defaults below apply exactly to the flags the caller
	// didn't set themselves, never overriding one they did (an explicit
	// -tcp-addr, in particular, must win over --dev's own default, not
	// the reverse).
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

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

	settingsService := api.NewSettingsService(api.NewSettingsStore(db), machineKey)

	logsDir := filepath.Join(cfg.stateDir, "jobs")
	jobStore := job.NewStore(db)
	logs := job.NewLogStore(logsDir)
	hub := job.NewHub()
	registry := job.NewRegistry()
	linuxDisks := disk.NewLinuxProvider()
	arrayStore := store.NewArrayStore(db)
	configRoot := cfg.configRoot
	if configRoot == "" {
		configRoot = "/etc"
	}
	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:  linuxDisks,
		Runner:    linuxDisks.Exec,
		Store:     arrayStore,
		Generator: cfggen.NewGenerator(configRoot),
		Mounter:   disk.SystemdMounter{Runner: linuxDisks.Exec},
	}))
	scheduler := job.NewScheduler(jobStore, logs, hub, registry)
	if err := scheduler.RecoverFromRestart(ctx); err != nil {
		return fmt.Errorf("recovering jobs after restart: %w", err)
	}

	arraySeq, err := newArraySequence(ctx, scheduler, arrayStore, linuxDisks, linuxDisks.Exec)
	if err != nil {
		return fmt.Errorf("building array stop/start sequence: %w", err)
	}

	handler := &api.Handler{Scheduler: scheduler, Store: jobStore, Logs: logs, Auth: authService, Notify: notifyService, Settings: settingsService, Disks: linuxDisks, Array: arraySeq}

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

	tcpListener, err := buildTCPListener(cfg)
	if err != nil {
		return fmt.Errorf("starting TCP listener: %w", err)
	}
	unixListener, err := setupUnixListener(cfg.socketPath)
	if err != nil {
		return fmt.Errorf("starting Unix socket listener: %w", err)
	}
	applySocketGroupPermissions(cfg.socketPath)

	pruneOnce(ctx, jobStore, logs, authStore)
	go runDailyPrune(ctx, jobStore, logs, authStore)
	go runNotifyDeliveryLoop(ctx, notifyService, notifyDeliveryInterval, notifyDeliveryBatchLimit)

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
	return runErr
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

func buildTCPListener(cfg config) (net.Listener, error) {
	ln, err := net.Listen("tcp", cfg.tcpAddr)
	if err != nil {
		return nil, err
	}
	filtered := newSourceFilteringListener(ln, cfg.allowAllSources)

	certPath := filepath.Join(cfg.stateDir, "tls", "hoserva.crt")
	keyPath := filepath.Join(cfg.stateDir, "tls", "hoserva.key")
	cert, err := loadOrGenerateTLSCertificate(certPath, keyPath)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	return newTLSSniffingListener(filtered, tlsConfig), nil
}

// pruneOnce runs every retention sweep this daemon does on its own state
// (never a data disk — CLAUDE.md's "nothing on a timer walks a data
// disk" doesn't apply to any of this): job logs (Q74) and expired
// sessions, which otherwise accumulate in the sessions table forever
// since nothing else ever deletes a row past its own expiry.
func pruneOnce(ctx context.Context, jobStore *job.Store, logs *job.LogStore, authStore *api.AuthStore) {
	active, err := activeJobIDs(ctx, jobStore)
	if err != nil {
		log.Printf("hoservad: listing active jobs for log retention: %v", err)
	} else if err := logs.Prune(time.Now(), active); err != nil {
		log.Printf("hoservad: pruning job logs: %v", err)
	}

	if err := authStore.DeleteExpiredSessions(ctx, time.Now()); err != nil {
		log.Printf("hoservad: pruning expired sessions: %v", err)
	}
}

func runDailyPrune(ctx context.Context, jobStore *job.Store, logs *job.LogStore, authStore *api.AuthStore) {
	ticker := time.NewTicker(logPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneOnce(ctx, jobStore, logs, authStore)
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
