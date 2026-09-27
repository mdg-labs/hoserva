package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
	"github.com/mdg-labs/hoserva/internal/update"

	_ "modernc.org/sqlite"
)

// contractNow is the fixed clock every contract-rig service uses, so a
// timestamp field's value is deterministic across runs (never compared
// itself — see contract_test.go's own note on response-body equality —
// but several validations key off "now" for relative ranges).
var contractNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// contractShareMounter is a no-op pool.Mounter: the contract rig never
// mounts anything real, and share.Service's own mutation logic (the
// validation this package contract-tests) never inspects the mount
// result.
type contractShareMounter struct{}

func (contractShareMounter) Mount(context.Context, pool.Mount) error { return nil }
func (contractShareMounter) Unmount(context.Context, string) error   { return nil }

// contractShareFS is a no-op share.FS: mockArrayDisks' own mountpoints
// ("/mnt/disk1", …) are kept literal (contract_rig_test.go's own note on
// newContractProductionHandler) so a contractCase can address the same
// disk by the same string on both handlers, but nothing under /mnt may
// ever be touched for real from this workspace (CLAUDE.md's "real disks
// are off-limits") — so share.Service's own branch-directory writes are
// against this in-memory no-op instead of share.OSFS.
type contractShareFS struct{}

func (contractShareFS) MkdirAll(string, os.FileMode) error       { return nil }
func (contractShareFS) Chmod(string, os.FileMode) error          { return nil }
func (contractShareFS) Chown(string, int, int) error             { return nil }
func (contractShareFS) RemoveAll(string) error                   { return nil }
func (contractShareFS) RemoveConfined(string, string) error      { return nil }
func (contractShareFS) ReadDir(string) ([]os.DirEntry, error)    { return nil, nil }
func (contractShareFS) Lstat(path string) (os.FileInfo, error)   { return os.Stat(os.DevNull) }
func (contractShareFS) EvalSymlinks(path string) (string, error) { return path, nil }
func (contractShareFS) GetXattr(string, string) ([]byte, error)  { return nil, nil }

// contractCipher is a minimal, reversible SettingsCipher/UPSSocketPermissions
// secret cipher, matching the pattern internal/api's own settings_handler_
// test.go and ups_handler_test.go fakes use — no real secret storage, just
// enough to round-trip a backup passphrase or a UPS monitor password
// through SettingsService/UPSService's own encrypt-before-store path.
type contractCipher struct{}

func (contractCipher) Encrypt(plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

func (contractCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return contractCipher{}.Encrypt(ciphertext)
}

// contractNUTReloader is UPSService's own NUTReloader fake, matching
// internal/api/ups_handler_test.go's fakeNUTReloader: no real NUT service
// exists in this rig, so Update's reload step always succeeds.
type contractNUTReloader struct{}

func (contractNUTReloader) Reload(context.Context, config.UPSConnection) error { return nil }

// contractUPSSocket is UPSService's own UPSSocketPermissions fake (#340),
// matching internal/api/ups_handler_test.go's fakeUPSSocketPermissions: no
// real ups control socket exists in this rig, so Apply always succeeds.
type contractUPSSocket struct{}

func (contractUPSSocket) Apply(context.Context) error { return nil }

// contractHTTPS is HTTPSControl's own fake, matching internal/api/
// network_handler_test.go's fakeHTTPS: no real TCP listener exists in this
// rig, so certificate/listen-port/access-scope state lives in memory only.
type contractHTTPS struct {
	allowAll bool
	port     int
	restart  bool
	notAfter time.Time
}

func (f *contractHTTPS) Certificate() (api.TLSCertView, error) {
	return api.TLSCertView{Kind: "self_signed", NotAfter: f.notAfter}, nil
}

func (f *contractHTTPS) Regenerate(context.Context) (api.TLSCertView, error) {
	f.notAfter = time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	return f.Certificate()
}

func (f *contractHTTPS) AllowAllSources() bool { return f.allowAll }
func (f *contractHTTPS) SetAllowAllSources(v bool) error {
	f.allowAll = v
	return nil
}
func (f *contractHTTPS) ListenPort() int { return f.port }
func (f *contractHTTPS) SetListenPort(port int) error {
	f.port = port
	f.restart = port != 8008
	return nil
}
func (f *contractHTTPS) ListenPortRestartRequired() bool { return f.restart }

// contractProductionRunFuncs are the job types the contract table submits.
// Submit's own admission logic (maintenance mode, class conflicts,
// disk-upgrade admission, ValidateParams) is what this package
// contract-tests, never a job's own run behaviour — so every type here
// gets a no-op RunFunc, and the topology types are never run from a real
// FakeProvider/FakeRunner/Mounter chain (newContractProductionHandler
// seeds ArrayStore directly with store.ArrayStore.PutArray instead).
var contractProductionRunFuncs = []job.Type{
	job.TypeDiskFormat,
	job.TypeDiskAdd,
	job.TypeDiskReplace,
	job.TypeDiskUpgradeData,
	job.TypeDiskUpgradeParity,
	job.TypeDiskRemove,
	job.TypeSync,
	job.TypeScrub,
	job.TypeFix,
	job.TypeMover,
	job.TypeRebalance,
	job.TypeEvacuation,
	job.TypeShareRelocation,
	job.TypeACMEIssue,
}

// contractDiskFromInventory converts one of mockDiskInventory's own
// entries to disk.Disk — the contract rig's FakeProvider reuses
// mockDiskInventory directly (never a second, hand-typed inventory) so
// the two handlers are validated against the identical disk set.
func contractDiskFromInventory(e apiv1.DiskInventoryEntry) disk.Disk {
	return disk.Disk{
		Device:          e.Device,
		Size:            e.SizeBytes,
		Model:           e.Model.Or(""),
		Serial:          e.Serial.Or(""),
		WWN:             e.Wwn.Or(""),
		Boot:            e.Boot,
		Failed:          e.Failed.Or(false),
		WeakIdentity:    e.WeakIdentity.Or(false),
		Filesystem:      e.Filesystem.Or(""),
		Label:           e.Label.Or(""),
		ContainsData:    e.ContainsData.Or(false),
		LooksLikeUnraid: e.LooksLikeUnraid.Or(false),
	}
}

// contractUpdateFixture is the signed release index both
// newContractProductionHandler's update.Engine and #272's update cases
// read — the same shape internal/api/update_handler_test.go's own
// newUpdateHandler builds (an ed25519-signed SHA256SUMS, one stable
// release for amd64), so CheckForUpdate/GetUpdateStatus/
// UpdateUpdateSettings exercise a real signature-verified fetch instead
// of a bare, always-failing engine.
type contractUpdateFixture struct {
	engine *update.Engine
	inst   *update.FakeInstaller
	jobs   *update.FakeJobs
}

func newContractUpdateEngine(t *testing.T, dbPath string) contractUpdateFixture {
	t.Helper()
	const indexURL = "https://hoserva.dev/releases/index.json"
	const debURL = "https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/hoserva_0.2.0_amd64.deb"
	deb := []byte("contract rig deb contents")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating update signing key: %v", err)
	}
	sum := sha256.Sum256(deb)
	hexSum := hex.EncodeToString(sum[:])
	sums := []byte(hexSum + "  hoserva_0.2.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)

	// A second release, at Engine.Current itself, so RollbackUpdate's
	// own case (ApplyUpdate to 0.2.0, then Rollback) has a previous
	// version the release index actually carries
	// (Engine.Rollback's own idx.findVersion(row.PreviousVersion)) —
	// production refuses a rollback target the index doesn't list, and
	// nothing about #272's own case setup should special-case that
	// refusal away.
	const prevDebURL = "https://github.com/mdg-labs/hoserva/releases/download/v0.1.0/hoserva_0.1.0_amd64.deb"
	prevDeb := []byte("contract rig previous deb contents")
	prevSum := sha256.Sum256(prevDeb)
	prevHexSum := hex.EncodeToString(prevSum[:])
	prevSums := []byte(prevHexSum + "  hoserva_0.1.0_amd64.deb\n")
	prevSig := ed25519.Sign(priv, prevSums)

	idx, err := json.Marshal(update.Index{Channels: map[update.Channel][]update.Release{
		update.ChannelStable: {
			{
				Tag:     "v0.2.0",
				Version: "0.2.0",
				Channel: update.ChannelStable,
				Assets:  map[string]update.Asset{"amd64": {URL: debURL, SHA256: hexSum}},
			},
			{
				Tag:     "v0.1.0",
				Version: "0.1.0",
				Channel: update.ChannelStable,
				Assets:  map[string]update.Asset{"amd64": {URL: prevDebURL, SHA256: prevHexSum}},
			},
		},
	}})
	if err != nil {
		t.Fatalf("encoding update index: %v", err)
	}
	sumsURL := strings.TrimSuffix(debURL, "hoserva_0.2.0_amd64.deb") + "SHA256SUMS"
	prevSumsURL := strings.TrimSuffix(prevDebURL, "hoserva_0.1.0_amd64.deb") + "SHA256SUMS"
	fetcher := &update.MapFetcher{Bodies: map[string][]byte{
		indexURL:             idx,
		debURL:               deb,
		sumsURL:              sums,
		sumsURL + ".sig":     sig,
		prevDebURL:           prevDeb,
		prevSumsURL:          prevSums,
		prevSumsURL + ".sig": prevSig,
	}}
	inst := &update.FakeInstaller{}
	jobs := &update.FakeJobs{}
	eng := &update.Engine{
		IndexURL:    indexURL,
		Arch:        "amd64",
		StateDir:    t.TempDir(),
		SnapshotDir: t.TempDir(),
		DBPath:      dbPath,
		Current:     "0.1.0",
		PublicKey:   pub,
		Fetcher:     fetcher,
		Installer:   inst,
		Host:        &update.FakeHost{Versions: map[string]string{"mergerfs": "2.40.2", "snapraid": "12.4"}},
		Jobs:        jobs,
		Backup:      &update.FakeBackup{},
		Notify:      &update.FakeNotifier{},
		Settings:    update.DefaultMemorySettings(),
	}
	return contractUpdateFixture{engine: eng, inst: inst, jobs: jobs}
}

// newContractProductionHandler builds a *api.Handler wired the same way
// hoservad's own main.go wires it (D18's "handlers implement the
// generated server interfaces" applies equally to this rig): a real
// SQLite database through the shared migration runner, an ArrayStore
// seeded from mockArrayDisks("healthy") and a FakeProvider seeded from
// mockDiskInventory("healthy") — the exact fixture the mock's own
// newHandler("healthy") reads — plus every business-logic service
// internal/api's own package tests build from fakes (share_handler_
// test.go, notify_handler_test.go, auth_handler_test.go, schedule_
// handler_test.go, settings_handler_test.go, ups_handler_test.go,
// network_handler_test.go, external_handler_test.go, hostconfig_test.go,
// metrics_handler_test.go, wake_events_handler_test.go, update_handler_
// test.go, parity_handler_test.go). Reusing mockArrayDisks/
// mockDiskInventory directly, instead of a second hand-typed fixture, is
// what keeps the two handlers' starting state identical as those mock
// fixtures change. scenario "fresh-install" seeds no admin account and no
// array topology, matching the mock's own fresh-install fixture — every
// other scenario seeds a single canned admin, matching mockAdminID
// (auth.go).
func newContractProductionHandler(t *testing.T, scenario string) *api.Handler {
	t.Helper()
	ctx := context.Background()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "contract-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening contract test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	arrayStore := store.NewArrayStore(db)
	if layout := mockArrayDisks(scenario); layout != nil {
		for i := range layout {
			// mockArrayDisks carries no FSUUID (the mock never checks
			// it); ArrayStore requires one per row to be unique. The
			// mountpoint itself is kept exactly as mockArrayDisks wrote
			// it — contractCases addresses disks by that same literal
			// mountpoint (e.g. "/mnt/disk1") on both handlers, and
			// nothing in this rig ever mounts anything for real.
			layout[i].FSUUID = "contract-" + layout[i].Device
		}
		if err := arrayStore.PutArray(ctx, store.ArraySettings{
			CreatePolicy: string(pool.DefaultCreatePolicy),
			MinFreeSpace: "50G",
			CreatedAt:    contractNow,
		}, layout); err != nil {
			t.Fatalf("seeding array topology: %v", err)
		}
		// PutArray's own InsertArrayDisk carries no removal_state column
		// (SetRemovalState is production's only writer of it, since a
		// disk only ever enters removal after create-array already ran)
		// — so a scenario's own removal state, set directly on the
		// layout above, has to be replayed here too, or this rig's own
		// disk would silently start "not in removal" while the mock
		// (which reads mockArrayDisks fresh on every call, never through
		// ArrayStore) still reports it correctly (#361).
		for _, d := range layout {
			if d.RemovalState == "" {
				continue
			}
			if err := arrayStore.SetRemovalState(ctx, d.Mountpoint, d.RemovalState, "contract-rig-removal"); err != nil {
				t.Fatalf("seeding removal state for %s: %v", d.Mountpoint, err)
			}
		}
	}

	provider := disk.NewFakeProvider()
	for _, e := range mockDiskInventory(scenario) {
		provider.AddDisk(e.Device, contractDiskFromInventory(e))
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)
	noop := func(context.Context, *job.RunContext) error { return nil }
	for _, jt := range contractProductionRunFuncs {
		registry.Register(jt, true, noop)
	}

	// degradedGate mirrors cmd/hoservad's own newArraySequence (#385, doc
	// 02 §1, Q69): expected from the same layout PutArray above seeded,
	// present from the same provider mockDiskInventory just populated —
	// so the "degraded" scenario's own disk4 (mockArrayDisks, empty
	// identity, never in mockDiskInventory) evaluates not ready on this
	// rig exactly as it does in production, with no scenario-specific
	// branch of its own here.
	expectedDisks := mockArrayDisks(scenario)
	expected := make([]disk.ExpectedDisk, 0, len(expectedDisks))
	for _, d := range expectedDisks {
		expected = append(expected, disk.ExpectedDisk{
			Identity: disk.Identity{WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity, ByIDName: d.ByIDName},
			Role:     d.Role,
			MountAt:  d.Mountpoint,
		})
	}
	degradedGate := disk.NewStorageGate(expected)
	listedForGate, err := provider.List(ctx)
	if err != nil {
		t.Fatalf("listing disks for the contract rig's storage gate: %v", err)
	}
	present := make([]disk.Identity, 0, len(listedForGate))
	for _, d := range listedForGate {
		present = append(present, disk.Identity{WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity, ByIDName: d.ByIDName})
	}
	degradedGate.Evaluate(present)

	shareRoot := filepath.Join(t.TempDir(), "user")
	if err := os.MkdirAll(shareRoot, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", shareRoot, err)
	}
	shareSvc := &share.Service{
		Shares:   store.NewShareStore(db),
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(t.TempDir(), "etc")),
		FS:       contractShareFS{},
		Mounter:  contractShareMounter{},
		Now:      func() time.Time { return contractNow },
		CatchAll: shareRoot,
	}

	notifySvc := notify.NewService(notify.NewStore(db), notify.FakeSecretCipher{}, notify.Senders{
		notify.ChannelEmail:   &notify.FakeSender{},
		notify.ChannelGotify:  &notify.FakeSender{},
		notify.ChannelNtfy:    &notify.FakeSender{},
		notify.ChannelDiscord: &notify.FakeSender{},
		notify.ChannelWebhook: &notify.FakeSender{},
	})
	notifySvc.Log = func(string, ...any) {}

	authStore := api.NewAuthStore(db)
	key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(t.TempDir(), "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	authSvc := api.NewAuthService(authStore, key)
	// NewAuthService defaults SambaAccounts to the real, smbpasswd-
	// exec'ing implementation (authservice.go's own doc comment);
	// internal/api's own handler tests replace it with
	// share.NewFakeSambaAccounts() so SetUserPassword/DeleteUser never
	// exec a binary this host may not have installed — this rig does
	// the same.
	authSvc.SambaAccounts = share.NewFakeSambaAccounts()
	if scenario != "fresh-install" {
		// mockAdminID's account (auth.go) is the mock's single canned
		// admin, present in every scenario but fresh-install — seeded
		// here by the same username so CreateApiToken/user-lookup cases
		// target a user that exists on both sides, and so
		// GetSetupStatus/CreateFirstAdmin's "already configured" refusal
		// is reachable the same way on both.
		if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
			t.Fatalf("seeding admin account: %v", err)
		}
	}

	scheduleStore := api.NewScheduleStore(db)
	settingsStore := api.NewSettingsStore(db)
	scheduleSvc := api.NewScheduleService(scheduleStore, settingsStore)
	settingsSvc := api.NewSettingsService(settingsStore, contractCipher{})

	upsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(upsRoot, "nut"), 0o755); err != nil {
		t.Fatalf("mkdir nut: %v", err)
	}
	upsGen := config.NewGenerator(upsRoot)
	upsGen.LookupGroup = func(name string) (int, error) {
		if name != config.NUTGroup {
			return 0, os.ErrNotExist
		}
		return os.Getgid(), nil
	}
	upsSvc := api.NewUPSService(api.NewUPSStore(db), contractCipher{}, upsGen, contractNUTReloader{}, contractUPSSocket{})

	netSvc := &config.NetworkService{
		Generator: config.NewGenerator(t.TempDir()),
		Detector:  config.MemoryDetector{Backend: config.BackendIfupdown},
		Runner:    &config.MemoryRunner{},
		// "enp1s0" matches the mock's own defaultMockNetwork
		// (network.go) — ConfirmNetworkSettings's own valid case (#272)
		// needs an interface ApplyNetworkSettings recognises on both
		// sides to reach a pending change at all.
		Links: config.MemoryLinks{Ifaces: []config.Iface{{
			Name: "enp1s0", Method: config.MethodDHCP, State: config.IfaceUp,
			Address: "10.0.2.15", Prefix: 24,
		}}},
		StateDir: t.TempDir(),
		Window:   time.Hour,
		Now:      func() time.Time { return contractNow },
	}
	t.Cleanup(netSvc.Close)
	https := &contractHTTPS{port: 8008, notAfter: contractNow.AddDate(10, 0, 0)}

	acmeDB := db
	acmeSvc := &acme.Service{Store: acme.NewStore(acmeDB), Cipher: acme.FakeCipher{}}

	hostRoot := t.TempDir()
	hostGen := config.NewGenerator(hostRoot)

	mount, err := metrics.Open(ctx, filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("opening metrics store: %v", err)
	}
	t.Cleanup(func() { _ = mount.Close() })

	updateFixture := newContractUpdateEngine(t, dbPath)

	extMounter := disk.NewFakeMounter()
	extRunner := disk.NewFakeRunner()
	// mockDiskInventory's own mockUSBDisk() entry (Label "backup",
	// Device /dev/sdf, Filesystem "xfs") is what FakeProvider.AddDisk
	// keeps for /dev/sdf once every mockDiskInventory entry has been
	// added (later entries win — mockUSBDisk is appended last) — the
	// same device the "spare disk" cases above resolve against, now
	// carrying a filesystem. RegisterExternalDisk/resolveExternal read
	// its real filesystem UUID through h.diskRunner() (external_handler.
	// go's own FilesystemUUID call) rather than falling back to a
	// pending one, so MountExternalDisk's own valid case can resolve a
	// disk that is not "format before mounting".
	extRunner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdf"}, []byte("ext-fixture-uuid\n"), nil)

	return &api.Handler{
		Scheduler:  scheduler,
		Store:      jobStore,
		Logs:       logs,
		Disks:      provider,
		ArrayStore: arrayStore,
		// ArrayReady: CancelDiskRemoval (#361) is the only handler method
		// that calls it directly rather than through a job — this rig
		// never mounts anything for real (Array's own doc comment
		// below), so a no-op that always succeeds matches every other
		// "mounts nothing for real" fake here.
		ArrayReady:  func(context.Context) error { return nil },
		Shares:      shareSvc,
		Notify:      notifySvc,
		Auth:        authSvc,
		Parity:      parity.NewFakeEngine(),
		ParityGuard: parity.Guard{},
		RebalanceShares: func(context.Context) ([]cache.Share, error) {
			return nil, nil
		},
		// Array is Q70's stop/start sequence (doc 02 §4): an empty
		// Services/ShareMounts/Disks/CatchAll here mounts and unmounts
		// nothing for real, but Stop/Start still run the Scheduler's own
		// maintenance-mode transition (EnterMaintenance, Drain,
		// MarkArrayStopped) — exactly the behaviour #330's contract
		// cases exercise. Gate is degradedGate above, built from this same
		// scenario's own topology and inventory (#385) — GetStatus's own
		// arrayDegraded and AcknowledgeDegradedArray both read it live.
		Array: &job.ArraySequence{Scheduler: scheduler, Gate: job.PendingUpgradeGate{Gate: degradedGate, Scheduler: scheduler}},
		// AcknowledgeDegraded (#385) calls disk.StorageGate.Acknowledge on
		// degradedGate directly — this rig mounts nothing for real (Array's
		// own doc comment above), so there is no storageTargetSync
		// transition to run here; only the status/error-code contract
		// AcknowledgeDegradedArray itself maps is in scope for this rig.
		// It still refuses with api.ErrDegradedServicesNotStarted while the
		// array is in maintenance mode (#385 finding 1) — the same case
		// storageTargetSync.UpdateOrError refuses in production — so this
		// rig's own "AcknowledgeDegradedArray"/"refused_when_array_is_in_
		// maintenance" contract case agrees with cmd/mockapi's own
		// maintenance-mode check instead of the mock refusing what this
		// rig used to accept.
		AcknowledgeDegraded: func(context.Context) error {
			if err := degradedGate.Acknowledge(); err != nil {
				return err
			}
			if scheduler.InMaintenance() {
				return fmt.Errorf("%w: the array is in maintenance mode", api.ErrDegradedServicesNotStarted)
			}
			return nil
		},
		Schedules:   scheduleSvc,
		Settings:    settingsSvc,
		UPS:         upsSvc,
		Network:     netSvc,
		HTTPS:       https,
		ACME:        acmeSvc,
		History:     store.NewHistory(db),
		Metrics:     mount,
		Updates:     updateFixture.engine,
		Generator:   hostGen,
		HostConfig:  store.NewHostConfigStore(db),
		Docker:      config.MemoryDocker{},
		DiskMounter: extMounter,
		DiskRunner:  extRunner,
	}
}

// newContractMockHandler is the mock side of the pair: newHandler's own
// scenario, the same fixture newContractProductionHandler mirrors above.
func newContractMockHandler(t *testing.T, scenario string) *handler {
	t.Helper()
	h, err := newHandler(scenario)
	if err != nil {
		t.Fatalf("newHandler(%q): %v", scenario, err)
	}
	return h
}

// newContractRig builds one production/mock pair from the same scenario
// fixture, both satisfying apiv1.Handler, so a contractCase's call
// closure runs unmodified against either side.
func newContractRig(t *testing.T, scenario string) (prod apiv1.Handler, mock apiv1.Handler) {
	t.Helper()
	return newContractProductionHandler(t, scenario), newContractMockHandler(t, scenario)
}
