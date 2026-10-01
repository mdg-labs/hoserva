package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
	"github.com/mdg-labs/hoserva/internal/template"
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
	job.TypeContainerRecreate,
	job.TypeContainerUpdate,
	job.TypeRestoreDrill,
	job.TypeConfigBackup,
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
// contractContainerProvider seeds a container.FakeProvider with the same
// container mockApps() (apps.go) reports, by ID and name, so a
// GetApp("jellyfin") contract case gets the identical 200/404 outcome on
// both handlers — this rig mounts nothing for real, so it is the only
// state either side has for Apps.
//
// appdata is a real temporary appdata root: production's shared-appdata
// check resolves mount sources on disk, so jellyfin and transcoder mount
// real directories — transcoder's inside jellyfin's, the way the mock's
// own two fixtures overlap.
func contractContainerProvider(t *testing.T, appdata string) *container.FakeProvider {
	t.Helper()
	jellyfinDir := filepath.Join(appdata, "jellyfin")
	transcodeDir := filepath.Join(jellyfinDir, "transcode")
	if err := os.MkdirAll(transcodeDir, 0o755); err != nil {
		t.Fatalf("creating contract appdata: %v", err)
	}
	f := container.NewFakeProvider()
	f.AddContainer(container.Container{
		ID:      "3f2a9c1e4b5d",
		Name:    "jellyfin",
		Image:   "lscr.io/linuxserver/jellyfin",
		Tag:     "10.9.7",
		ImageID: "sha256:jellyfin",
		State:   "running",
		Status:  "Up 3 hours",
		Mounts:  []container.Mount{{Source: jellyfinDir, Destination: "/config", ReadWrite: true}},
	})
	f.AddContainer(container.Container{
		ID:     "4c8e0d2a7b91",
		Name:   "transcoder",
		Image:  "example/transcoder",
		Tag:    "1.4.0",
		Pinned: true,
		State:  "exited",
		Status: "Exited (0) 5 hours ago",
		Mounts: []container.Mount{{Source: transcodeDir, Destination: "/transcode", ReadWrite: true}},
	})
	postgresDir := filepath.Join(appdata, "postgres")
	if err := os.MkdirAll(postgresDir, 0o755); err != nil {
		t.Fatalf("creating contract appdata: %v", err)
	}
	f.AddContainer(container.Container{
		ID:     "7d3f1a9e5c20",
		Name:   "postgres",
		Image:  "postgres",
		Tag:    "16.4",
		State:  "running",
		Status: "Up 3 hours",
		Mounts: []container.Mount{{Source: postgresDir, Destination: "/var/lib/postgresql/data", ReadWrite: true}},
	})
	f.AddContainer(container.Container{
		ID:     "9b1d7e2f6a3c",
		Name:   "portainer",
		Image:  "portainer/portainer-ce",
		Tag:    "2.21.4",
		State:  "exited",
		Status: "Exited (0) 2 days ago",
	})
	return f
}

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

	// Registered last, so t.Cleanup's LIFO order runs this before every
	// other cleanup above — the db.Close, the netSvc.Close, the mount.Close
	// and every t.TempDir() removal registered while building this handler.
	// A contractCase (e.g. StartMover/valid) can submit a real job and
	// return before it finishes; without this, the job's own goroutine can
	// still be writing to jobStore (through db) or to logs' t.TempDir()
	// when those get closed or removed out from under it (#397).
	t.Cleanup(func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := scheduler.Drain(drainCtx); err != nil {
			t.Errorf("draining contract rig jobs before cleanup: %v", err)
		}
	})

	appdata := filepath.Join(t.TempDir(), "appdata")
	containers := contractContainerProvider(t, appdata)
	h := &api.Handler{
		Scheduler: scheduler,
		Store:     jobStore,
		Logs:      logs,
		Disks:     provider,
		Container: containers,
		// Like hoservad's appdataRoots (cmd/hoservad/containers.go), the
		// appdata location exists only when the array has a cache disk
		// (container.CacheAppdataRoots, the same rule); it is then a
		// temporary directory here, standing for the mock's
		// mockAppdataRoot, so no case deletes anything outside it. No
		// scenario has a cache disk, so this rig never reaches the
		// deletion itself — only the appdata_unavailable refusal.
		Lifecycle: &container.Lifecycle{
			Provider: containers,
			// The array state hoservad wires (cmd/hoservad/containers.go):
			// the scheduler's maintenance mode and, standing in for the
			// storage target this rig does not have, the storage gate —
			// which, like hoservad with no array configured
			// (newArraySequence returns nil), is never ready when the
			// scenario has no array.
			Halted: scheduler.InMaintenance,
			StorageReady: func() bool {
				return len(expected) > 0 && degradedGate.Ready()
			},
			AppdataRoots: func(ctx context.Context) ([]string, error) {
				_, disks, err := arrayStore.GetArray(ctx)
				if err != nil {
					if errors.Is(err, store.ErrNoArray) {
						return nil, nil
					}
					return nil, err
				}
				if container.CacheAppdataRoots(disks) == nil {
					return nil, nil
				}
				return []string{appdata}, nil
			},
		},
		// Stacks is the real StackService over this rig's migrated
		// database, a temporary stacks directory and a scripted docker
		// compose, so no case runs a real container or writes outside it.
		Stacks: &container.StackService{
			Store:  store.NewStackStore(db),
			Cipher: contractStackCipher{},
			Runner: container.NewFakeRunner(),
			Root:   filepath.Join(t.TempDir(), "stacks"),
		},
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
		// StorageServicesReleased stands in for hoservad's
		// storageTargetSync.Ready (#385): this rig has no storageTargetSync,
		// so it reads the same gate StorageReady does. Like hoservad with no
		// array configured, it is never true when the scenario has none.
		StorageServicesReleased: func() bool {
			return len(expected) > 0 && degradedGate.Ready()
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
		// Backup (#269) is a minimal, real backup.Service against this
		// rig's own migrated db/dbPath — enough for ImportConfig's own
		// checksum/integrity/schema-version validation to run and for its
		// pre-restore h.Backup.Run(ctx) to succeed (Destinations is empty,
		// so nothing here is written to any real path). ExportConfig
		// stays out of contractCases (contractSkip's own entry) since the
		// mock's own stub bytes give it no failure path to compare.
		//
		// Its destinations (#60) are the mock's two seeded ones, "boot"
		// and "pool", with paths in this test's own temp directory so
		// nothing is written to a real path, and a fake rclone.
		Backup: contractBackupService(t, db, dbPath),
		// The regeneration step ImportConfig runs once the database is
		// restored; an archive that does not verify is refused before it.
		RegenerateConfig: func(context.Context) error { return nil },
		// The bare-metal restore's extra hook (doc 10 §1): it writes the
		// disk mount units and snapraid.conf, which nothing here has.
		RegenerateArray: func(context.Context) error { return nil },
	}
	if scenario == "fresh-install" {
		prepareContractBareMetal(t, db, h.Backup, dbPath)
	}
	// Appdata backup (#61) is the real service over this rig's own
	// Docker fake and backup destinations. This rig has no cache disk in any
	// scenario, so its appdata location is the temporary directory the
	// containers above already mount, standing for the mock's
	// mockAppdataRoot; the scope, the refusals and the job submission are
	// what is compared. The three job types run the real backup, restore and preview
	// (not the no-op the other types get), so a case can restore an archive
	// a backup it started has written.
	appdataSvc := &backup.AppdataService{
		Backup:      h.Backup,
		Containers:  backup.LifecycleContainers{Lifecycle: h.Lifecycle},
		Roots:       func(context.Context) ([]string, error) { return []string{appdata}, nil },
		Policies:    api.NewAppdataPolicyStore(db),
		JournalPath: filepath.Join(t.TempDir(), "appdata-stopped.json"),
	}
	h.Appdata = appdataSvc
	// Container updates (#284) are the real Updater over this rig's Docker
	// fake, history in the migrated database and the appdata backup's own
	// snapshot mechanism, with the update check's stored results as the
	// source of a bulk update's targets. The job type itself runs no-op like
	// the rest, so no update is ever recorded here.
	updateChecker := &container.UpdateChecker{Provider: containers, Results: store.NewUpdateStore(db)}
	h.AppUpdates = updateChecker
	h.AppUpdater = &container.Updater{
		Lifecycle: h.Lifecycle,
		History:   store.NewImageHistoryStore(db),
		Snapshots: backup.UpdateSnapshots{Appdata: appdataSvc},
		Statuses:  updateChecker,
	}
	// The stacks take the array check, appdata location and container
	// listing hoservad's wireStacks gives them: the same Lifecycle's.
	stacks := h.Stacks
	stacks.RequireArrayRunning = h.Lifecycle.RequireArrayRunning
	stacks.Provider = h.Lifecycle.Provider
	stacks.AppdataRoots = h.Lifecycle.AppdataRoots
	// Template install resolves the mock's own catalog through the real
	// installer, against this rig's Docker fake and an empty host socket
	// table, so the host's own listeners never decide a case.
	procNet := t.TempDir()
	for _, f := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if err := os.WriteFile(filepath.Join(procNet, f), []byte("  sl  local_address rem_address   st\n"), 0o644); err != nil {
			t.Fatalf("writing the empty %s socket table: %v", f, err)
		}
	}
	h.TemplateInstall = &template.Installer{
		Catalog: template.MapCatalog{Source: template.SourceCurated, Templates: mockTemplates},
		Stacks:  stacks,
		Ports:   template.HostPorts{Containers: containers, ProcNet: procNet},
		Shares: func(ctx context.Context) ([]string, error) {
			list, err := h.Shares.List(ctx)
			names := make([]string, len(list))
			for i, s := range list {
				names[i] = s.Name
			}
			return names, err
		},
		GPU:      mockGPU{},
		Timezone: func() string { return "UTC" },
	}
	registry.Register(job.TypeAppdataBackup, true, job.RunAppdataBackup(job.AppdataBackupDeps{
		Backup: func(ctx context.Context, requested, resolved []string, out io.Writer) error {
			return appdataSvc.Run(ctx, backup.AppdataRunRequest{Containers: requested, Resolved: resolved}, out)
		},
	}))
	registry.Register(job.TypeAppdataRestore, false, job.RunAppdataRestore(func(ctx context.Context, p job.AppdataRestoreParams, out io.Writer) error {
		return appdataSvc.Restore(ctx, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID}, out)
	}))
	registry.Register(job.TypeAppdataRestorePreview, true, job.RunAppdataRestorePreview(func(ctx context.Context, id string, p job.AppdataRestoreParams, out io.Writer) error {
		return appdataSvc.RunPreview(ctx, id, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID}, out)
	}))
	return h
}

// prepareContractBareMetal readies the fresh-install rig for a bare-metal
// restore that runs to the end: the installation's own backup recipient,
// which the restore keeps beside the machine key check the rig already has,
// and every path the restore writes under a temporary directory instead of
// hoservad's own.
func prepareContractBareMetal(t *testing.T, db *sql.DB, svc *backup.Service, dbPath string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at) VALUES (1, 'age1contract', x'bb', x'cc', '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatalf("seeding the contract rig's backup recipient: %v", err)
	}
	paths := backup.DefaultPaths(t.TempDir(), t.TempDir())
	paths.DBPath = dbPath
	svc.Paths = paths
}

func contractBackupService(t *testing.T, db *sql.DB, dbPath string) *backup.Service {
	t.Helper()
	defaultPaths := backup.DefaultPaths("", "")
	svc := &backup.Service{
		DB: db,
		// StateDir and ConfigRoot are only the protected-path inputs of
		// destination admission; DBPath is what the service writes to.
		Paths:             backup.Paths{DBPath: dbPath, StateDir: defaultPaths.StateDir, ConfigRoot: defaultPaths.ConfigRoot},
		Store:             api.NewBackupDestinationStore(db),
		Drills:            api.NewDrillStore(db),
		Rclone:            &backup.FakeRclone{},
		Cipher:            backup.FakeSecretCipher{},
		DestinationCipher: backup.FakeSecretCipher{},
	}
	defaults := mockBackupDestinations()
	root := t.TempDir()
	for i := range defaults {
		defaults[i].Path = filepath.Join(root, defaults[i].ID)
	}
	if err := svc.SeedDestinations(context.Background(), defaults); err != nil {
		t.Fatalf("seeding contract backup destinations: %v", err)
	}
	return svc
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

// seedJellyfinUpdate gives jellyfin a recorded update on whichever side h is,
// as a finished container_update job would have left it: this rig runs that
// job type no-op, so neither side has one otherwise. keep is how long from
// now the previous image is kept; a negative one is a keep period already
// over. Production's record also has its kept image on the Docker fake.
func seedJellyfinUpdate(ctx context.Context, h apiv1.Handler, keep time.Duration) error {
	const image = "lscr.io/linuxserver/jellyfin:10.9.7"
	now := time.Now().UTC()
	switch s := h.(type) {
	case *api.Handler:
		rec, err := s.AppUpdater.History.InsertImageHistory(ctx, store.ImageHistory{
			Container: "jellyfin", Image: image, PreviousImageID: "sha256:jellyfin-previous",
			UpdatedAt: now.Add(-time.Hour), KeepUntil: now.Add(keep),
		})
		if err != nil {
			return err
		}
		s.Lifecycle.Provider.(*container.FakeProvider).AddImage(container.Image{ID: "sha256:jellyfin-previous", RepoTags: []string{container.KeepRef(rec.ID)}})
		return nil
	case *handler:
		s.appsMu.Lock()
		defer s.appsMu.Unlock()
		s.updateRecords = append(s.updateRecords, apiv1.AppUpdateRecord{
			ID: int64(len(s.updateRecords) + 1), Container: "jellyfin", Image: image, PreviousImageId: "sha256:jellyfin-previous",
			UpdatedAt: now.Add(-time.Hour), KeepUntil: now.Add(keep), Revertible: keep > 0,
		})
		return nil
	}
	return fmt.Errorf("unexpected handler type %T", h)
}

// contractStackCipher stands in for the machine key: the contract compares
// status and error code, never the sealed bytes.
type contractStackCipher struct{}

func (contractStackCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil }
func (contractStackCipher) Decrypt(c []byte) ([]byte, error) { return append([]byte(nil), c...), nil }
