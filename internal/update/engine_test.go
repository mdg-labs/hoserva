package update

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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

const (
	testIndexURL = "https://hoserva.dev/releases/index.json"
	testDebURL   = "https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/hoserva_0.2.0_amd64.deb"
	testPrevURL  = "https://github.com/mdg-labs/hoserva/releases/download/v0.1.0/hoserva_0.1.0_amd64.deb"
)

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	deb  []byte
	sum  string
	idx  []byte
	sums []byte
	sig  []byte
}

func newFixture(t *testing.T, debBody []byte, indexSHA string) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(debBody)
	hexSum := hex.EncodeToString(sum[:])
	if indexSHA == "" {
		indexSHA = hexSum
	}
	sums := []byte(hexSum + "  hoserva_0.2.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)
	idx, err := json.Marshal(Index{Channels: map[Channel][]Release{
		ChannelStable: {{
			Tag:     "v0.2.0",
			Version: "0.2.0",
			Channel: ChannelStable,
			Assets: map[string]Asset{
				"amd64": {URL: testDebURL, SHA256: indexSHA},
				"arm64": {URL: strings.Replace(testDebURL, "amd64", "arm64", 1), SHA256: indexSHA},
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pub: pub, priv: priv, deb: debBody, sum: hexSum, idx: idx, sums: sums, sig: sig}
}

func (f fixture) fetcher() *MapFetcher {
	sumsURL, sigURL, _ := sumsURLs(testDebURL)
	return &MapFetcher{Bodies: map[string][]byte{
		testIndexURL: f.idx,
		testDebURL:   f.deb,
		sumsURL:      f.sums,
		sigURL:       f.sig,
	}}
}

func testEngine(t *testing.T, f fixture) (*Engine, *FakeInstaller, *FakeNotifier, *FakeBackup, *FakeJobs, *FakeHost) {
	t.Helper()
	inst := &FakeInstaller{}
	notes := &FakeNotifier{}
	backup := &FakeBackup{}
	jobs := &FakeJobs{}
	host := &FakeHost{Versions: map[string]string{"mergerfs": "2.40.2-5", "snapraid": "12.4-1"}}
	e := &Engine{
		IndexURL:  testIndexURL,
		Arch:      "amd64",
		StateDir:  t.TempDir(),
		Current:   "0.1.0",
		PublicKey: f.pub,
		Fetcher:   f.fetcher(),
		Installer: inst,
		Host:      host,
		Jobs:      jobs,
		Backup:    backup,
		Notify:    notes,
		Settings:  DefaultMemorySettings(),
		Snapshots: &recordingSnapshots{live: filepath.Join(t.TempDir(), "hoserva-pre-migration-v00000000000001-0000000001.db")},
	}
	return e, inst, notes, backup, jobs, host
}

func TestApply_ChecksumMismatchInstallsNothingAndNotifies(t *testing.T) {
	ctx := context.Background()
	good := []byte("hoserva-deb-v0.2.0")
	f := newFixture(t, good, "")
	tampered := []byte("hoserva-deb-TAMPERED")
	f.deb = tampered
	e, inst, notes, backup, _, host := testEngine(t, f)
	e.Fetcher = f.fetcher()

	err := e.Apply(ctx)
	if err == nil || !isChecksumErr(err) {
		t.Fatalf("Apply = %v, want checksum mismatch", err)
	}
	if len(inst.Calls) != 0 {
		t.Fatalf("installer called %v — a failed checksum must install nothing", inst.Calls)
	}
	if backup.Runs != 1 {
		t.Fatalf("backup runs = %d, want 1 (config backup still runs before download)", backup.Runs)
	}
	if !notes.notifiedFailure() {
		t.Fatalf("failed checksum did not notify: %v", notes.Titles)
	}
	if len(notes.Events) != 1 || notes.Events[0] != notify.EventHoservaUpdateFailed {
		t.Fatalf("notify events = %v, want [%s]", notes.Events, notify.EventHoservaUpdateFailed)
	}
	severity, ok := notify.DefaultSeverity(notify.EventHoservaUpdateFailed)
	if !ok || severity != notify.SeverityError {
		t.Fatalf("hoserva_update_failed severity = %q, want error", severity)
	}
	if host.RebootCalls != 0 {
		t.Fatalf("Apply rebooted (%d) — Hoserva never reboots on its own", host.RebootCalls)
	}
}

func TestApply_IndexSHAMismatchInstallsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	e, inst, notes, _, _, _ := testEngine(t, f)

	err := e.Apply(ctx)
	if err == nil || !isChecksumErr(err) {
		t.Fatalf("Apply = %v, want checksum mismatch when index sha256 disagrees with signed SHA256SUMS", err)
	}
	if len(inst.Calls) != 0 {
		t.Fatalf("installer called %v", inst.Calls)
	}
	if !notes.notifiedFailure() {
		t.Fatalf("did not notify: %v", notes.Titles)
	}
	for _, event := range notes.Events {
		if event == notify.EventHoservaUpdateAvailable {
			t.Fatalf("checksum failure published %s, want %s", event, notify.EventHoservaUpdateFailed)
		}
	}
}

func TestApply_RefusedWhileStorageJobRunning(t *testing.T) {
	cases := []struct {
		name  string
		typ   job.Type
		class job.Class
	}{
		{"parity", job.TypeSync, job.ClassParity},
		{"array_write", job.TypeMover, job.ClassArrayWrite},
		{"topology", job.TypeDiskAdd, job.ClassTopology},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, []byte("deb"), "")
			e, inst, _, backup, jobs, _ := testEngine(t, f)
			jobs.Blocking = &job.Job{ID: "11111111-1111-4111-8111-111111111111", Type: tc.typ, Class: tc.class}

			err := e.Apply(ctx)
			var blocked ErrBlocked
			if err == nil {
				t.Fatal("Apply succeeded while a storage job was running")
			}
			if !asBlocked(err, &blocked) {
				t.Fatalf("Apply = %v, want ErrBlocked", err)
			}
			if blocked.Job == nil || blocked.Job.ID != jobs.Blocking.ID {
				t.Fatalf("blocked job = %+v, want the running %s job", blocked.Job, tc.class)
			}
			if !strings.Contains(err.Error(), jobs.Blocking.ID) || !strings.Contains(err.Error(), string(tc.class)) {
				t.Fatalf("refusal %q does not name the blocking job", err)
			}
			if len(inst.Calls) != 0 {
				t.Fatalf("installer called despite refusal: %v", inst.Calls)
			}
			if backup.Runs != 0 {
				t.Fatal("config backup ran after a job-class refusal")
			}
		})
	}
}

func TestRollback_RestoresPreMigrationSnapshot(t *testing.T) {
	ctx := context.Background()
	prevDeb := []byte("hoserva-deb-v0.1.0")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(prevDeb)
	hexSum := hex.EncodeToString(sum[:])
	sums := []byte(hexSum + "  hoserva_0.1.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)
	idx, err := json.Marshal(Index{Channels: map[Channel][]Release{
		ChannelStable: {
			{Tag: "v0.2.0", Version: "0.2.0", Channel: ChannelStable, Assets: map[string]Asset{
				"amd64": {URL: testDebURL, SHA256: hexSum},
			}},
			{Tag: "v0.1.0", Version: "0.1.0", Channel: ChannelStable, Assets: map[string]Asset{
				"amd64": {URL: testPrevURL, SHA256: hexSum},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sumsURL, sigURL, _ := sumsURLs(testPrevURL)
	fetcher := &MapFetcher{Bodies: map[string][]byte{
		testIndexURL: idx,
		testPrevURL:  prevDeb,
		sumsURL:      sums,
		sigURL:       sig,
	}}
	state := t.TempDir()
	snapDir := filepath.Join(state, "backups", "pre-migration")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inst := &FakeInstaller{}
	liveSnap := filepath.Join(snapDir, "hoserva-pre-migration-v20260919223737-0000000001.db")
	if err := os.WriteFile(liveSnap, []byte("live-rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(snapDir, "hoserva-pre-migration-v99999999999999-0000000001.db")
	if err := os.WriteFile(decoy, []byte("newer-unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRollbackTarget(snapDir, liveSnap); err != nil {
		t.Fatal(err)
	}
	snaps := &recordingSnapshots{live: liveSnap}
	settings := DefaultMemorySettings()
	settings.Row.PreviousVersion = "0.1.0"
	e := &Engine{
		IndexURL:    testIndexURL,
		Arch:        "amd64",
		StateDir:    state,
		SnapshotDir: snapDir,
		Current:     "0.2.0",
		PublicKey:   pub,
		Fetcher:     fetcher,
		Installer:   inst,
		Host:        &FakeHost{Versions: map[string]string{"mergerfs": "2.40.2", "snapraid": "12.4"}},
		Jobs:        &FakeJobs{},
		Backup:      &FakeBackup{},
		Snapshots:   snaps,
		Settings:    settings,
		Notify:      &FakeNotifier{},
	}

	if err := e.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(inst.Calls) != 1 {
		t.Fatalf("installer calls = %v, want 1", inst.Calls)
	}
	marker := filepath.Join(e.pendingDir(), pendingSnapshotName)
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("snapshot marker: %v", err)
	}
	if strings.TrimSpace(string(got)) != liveSnap {
		t.Fatalf("snapshot marker = %q, want apply-time snapshot %q (not LatestSnapshot %q)", got, liveSnap, decoy)
	}
}

// TestRollback_DoesNotRestoreFirstInstallSnapshotAfterNoMigrationUpgrade
// is the Q67 data-loss scenario: first boot snapshots the empty database,
// the admin then writes rows, and a later upgrade has no schema change so
// startup takes no new snapshot. Rollback must restore the live database
// as it was at Apply — not LatestSnapshot, which is still that empty
// first-install file.
func TestRollback_DoesNotRestoreFirstInstallSnapshotAfterNoMigrationUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "hoserva.db")
	snapDir := filepath.Join(dir, "pre-migration")

	live, err := sql.Open("sqlite", store.DSN(livePath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	emptySnap, err := store.Snapshot(ctx, live, snapDir, "00000000000000")
	if err != nil {
		t.Fatalf("first-install snapshot: %v", err)
	}
	if _, err := live.ExecContext(ctx, `INSERT INTO items (id, name) VALUES (1, 'keep-me'), (2, 'also-keep')`); err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version TEXT PRIMARY KEY,
		slug TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `INSERT INTO schema_migrations (version, slug, checksum, applied_at)
		VALUES ('20260919223737', 'add_update_settings', 'abc', '2026-09-19')`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	prevDeb := []byte("hoserva-deb-v0.1.0")
	nextDeb := []byte("hoserva-deb-v0.2.0")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prevSum := sha256.Sum256(prevDeb)
	nextSum := sha256.Sum256(nextDeb)
	prevHex := hex.EncodeToString(prevSum[:])
	nextHex := hex.EncodeToString(nextSum[:])
	prevSums := []byte(prevHex + "  hoserva_0.1.0_amd64.deb\n")
	nextSums := []byte(nextHex + "  hoserva_0.2.0_amd64.deb\n")
	idx, err := json.Marshal(Index{Channels: map[Channel][]Release{
		ChannelStable: {
			{Tag: "v0.2.0", Version: "0.2.0", Channel: ChannelStable, Assets: map[string]Asset{
				"amd64": {URL: testDebURL, SHA256: nextHex},
			}},
			{Tag: "v0.1.0", Version: "0.1.0", Channel: ChannelStable, Assets: map[string]Asset{
				"amd64": {URL: testPrevURL, SHA256: prevHex},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	nextSumsURL, nextSigURL, _ := sumsURLs(testDebURL)
	prevSumsURL, prevSigURL, _ := sumsURLs(testPrevURL)
	state := t.TempDir()
	e := &Engine{
		IndexURL:    testIndexURL,
		Arch:        "amd64",
		StateDir:    state,
		SnapshotDir: snapDir,
		DBPath:      livePath,
		Current:     "0.1.0",
		PublicKey:   pub,
		Fetcher: &MapFetcher{Bodies: map[string][]byte{
			testIndexURL: idx,
			testDebURL:   nextDeb,
			testPrevURL:  prevDeb,
			nextSumsURL:  nextSums,
			nextSigURL:   ed25519.Sign(priv, nextSums),
			prevSumsURL:  prevSums,
			prevSigURL:   ed25519.Sign(priv, prevSums),
		}},
		Installer: &FakeInstaller{},
		Host:      &FakeHost{Versions: map[string]string{"mergerfs": "2.40.2", "snapraid": "12.4"}},
		Jobs:      &FakeJobs{},
		Backup:    &FakeBackup{},
		Settings:  DefaultMemorySettings(),
		Notify:    &FakeNotifier{},
	}

	if err := e.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	decoyDB, err := sql.Open("sqlite", filepath.Join(dir, "decoy.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoyDB.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	decoySnap, err := store.Snapshot(ctx, decoyDB, snapDir, "99999999999999")
	if err != nil {
		t.Fatalf("decoy snapshot: %v", err)
	}
	if err := decoyDB.Close(); err != nil {
		t.Fatal(err)
	}

	if err := e.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	marker := filepath.Join(e.pendingDir(), pendingSnapshotName)
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("snapshot marker: %v", err)
	}
	restorePath := strings.TrimSpace(string(got))
	if restorePath == emptySnap || restorePath == decoySnap {
		t.Fatalf("Rollback restored %q — the empty first-install snapshot or a later LatestSnapshot, not the live database from Apply", restorePath)
	}
	latest, err := store.LatestSnapshot(snapDir)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if restorePath == latest {
		t.Fatalf("Rollback used LatestSnapshot %q rather than the apply-time live snapshot", latest)
	}

	copyPath := filepath.Join(dir, "restored.db")
	if err := store.RestoreSnapshot(ctx, copyPath, restorePath); err != nil {
		t.Fatalf("RestoreSnapshot of rollback target: %v", err)
	}
	restored, err := sql.Open("sqlite", store.DSN(copyPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	var names []string
	rows, err := restored.QueryContext(ctx, `SELECT name FROM items ORDER BY id`)
	if err != nil {
		t.Fatalf("querying restored items: %v — rollback restored a snapshot from before the previous package's lifetime", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"keep-me", "also-keep"}
	if len(names) != len(want) {
		t.Fatalf("restored rows = %v, want %v — Rollback selected a snapshot from before the previous package's lifetime", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("restored rows = %v, want %v", names, want)
		}
	}
}

func TestReboot_WaitsThenShutdownNeverAuto(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, inst, _, _, jobs, host := testEngine(t, f)
	shutdown := &FakeShutdown{}
	e.Shutdown = shutdown

	if err := e.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if host.RebootCalls != 0 {
		t.Fatal("Apply called Host.Reboot — Hoserva never reboots on its own")
	}
	if shutdown.Stops != 0 {
		t.Fatal("Apply ran the shutdown sequence")
	}

	if err := e.Reboot(ctx); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if jobs.WaitN != 1 {
		t.Fatalf("WaitForStorageJobs calls = %d, want 1", jobs.WaitN)
	}
	if shutdown.Stops != 1 {
		t.Fatalf("Shutdown.Stop calls = %d, want 1", shutdown.Stops)
	}
	if host.RebootCalls != 1 {
		t.Fatalf("Reboot calls = %d, want 1", host.RebootCalls)
	}
	if len(inst.Calls) != 1 {
		t.Fatalf("installer should have been called only by Apply, not Reboot: %v", inst.Calls)
	}
}

func TestReboot_DataDiskUpgradeRunsStopSequenceInsteadOfWaiting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, _, _, _, jobs, host := testEngine(t, f)
	shutdown := &FakeShutdown{}
	e.Shutdown = shutdown
	jobs.Blocking = &job.Job{ID: "upgrade-1", Type: job.TypeDiskUpgradeData}

	if err := e.Reboot(ctx); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if jobs.WaitN != 0 {
		t.Fatalf("WaitForStorageJobs calls = %d, want 0 — a running data-disk upgrade must not be waited for (Q68 vs doc 02 §4 E4)", jobs.WaitN)
	}
	if shutdown.Stops != 1 {
		t.Fatalf("Shutdown.Stop calls = %d, want 1 — the stop sequence is what stops the upgrade at its checkpoint", shutdown.Stops)
	}
	if host.RebootCalls != 1 {
		t.Fatalf("Reboot calls = %d, want 1", host.RebootCalls)
	}
}

func TestReboot_OtherStorageJobStillWaits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, _, _, _, jobs, _ := testEngine(t, f)
	shutdown := &FakeShutdown{}
	e.Shutdown = shutdown
	jobs.Blocking = &job.Job{ID: "sync-1", Type: job.TypeSync}

	if err := e.Reboot(ctx); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if jobs.WaitN != 1 {
		t.Fatalf("WaitForStorageJobs calls = %d, want 1 — a running sync keeps Q68's wait", jobs.WaitN)
	}
}

func TestCheck_NeverUsesGitHubAPI(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, _, _, _, _, _ := testEngine(t, f)
	if _, err := e.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	fetcher := e.Fetcher.(*MapFetcher)
	for _, url := range fetcher.Hits {
		if strings.Contains(url, "api.github.com") {
			t.Fatalf("update check fetched GitHub API URL %s", url)
		}
	}
	if len(fetcher.Hits) != 1 || fetcher.Hits[0] != testIndexURL {
		t.Fatalf("check hits = %v, want only the signed index", fetcher.Hits)
	}
}

func TestApply_NeverCallsAptUpdate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	runner := &recordingRunner{}
	e, _, _, _, _, _ := testEngine(t, f)
	e.Host = DebianHost{Runner: runner, RebootRequiredPath: filepath.Join(t.TempDir(), "reboot-required")}
	if _, err := e.Status(ctx, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, c := range runner.calls {
		if strings.Contains(c, "apt update") || strings.Contains(c, "apt-get update") {
			t.Fatalf("Status ran %q — update check must never apt update", c)
		}
	}
}

func TestApplyVerifiedUpdate_RefusesTamperedPendingDeb(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pendingDebName), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pendingHashName), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var installed string
	err := ApplyVerifiedUpdate(ctx, dir, filepath.Join(dir, "hoserva.db"), func(context.Context, string, string) error {
		t.Fatal("restore ran after a checksum failure")
		return nil
	}, func(_ context.Context, debPath string) error {
		installed = debPath
		return nil
	})
	if err == nil || !isChecksumErr(err) {
		t.Fatalf("ApplyVerifiedUpdate = %v, want checksum mismatch", err)
	}
	if installed != "" {
		t.Fatalf("dpkg ran on %s after checksum failure", installed)
	}
}

func TestApplyVerifiedUpdate_RestoresThenInstalls(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	deb := []byte("verified-deb")
	sum := sha256.Sum256(deb)
	if err := os.WriteFile(filepath.Join(dir, pendingDebName), deb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pendingHashName), []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "hoserva-pre-migration-v00000000000001-0000000001.db")
	if err := os.WriteFile(filepath.Join(dir, pendingSnapshotName), []byte(snap+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var restored, installed string
	if err := ApplyVerifiedUpdate(ctx, dir, filepath.Join(dir, "live.db"), func(_ context.Context, live, snapshot string) error {
		restored = snapshot
		if live == "" {
			t.Fatal("restore live path empty")
		}
		return nil
	}, func(_ context.Context, debPath string) error {
		if restored == "" {
			t.Fatal("dpkg ran before snapshot restore")
		}
		installed = debPath
		return nil
	}); err != nil {
		t.Fatalf("ApplyVerifiedUpdate: %v", err)
	}
	if restored != snap {
		t.Fatalf("restored %q, want %q", restored, snap)
	}
	if installed != filepath.Join(dir, pendingDebName) {
		t.Fatalf("installed %q", installed)
	}
}

func TestStatus_ReportsDebianPendingAndRebootRequired(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, _, _, _, _, host := testEngine(t, f)
	host.NeedReboot = true
	host.Pending = []PendingUpdate{{Name: "openssl", InstalledVersion: "3.0.0", CandidateVersion: "3.0.1"}}
	st, err := e.Status(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RebootRequired {
		t.Fatal("rebootRequired = false")
	}
	if len(st.PendingDebianUpdates) != 1 || st.PendingDebianUpdates[0].Name != "openssl" {
		t.Fatalf("pending = %+v", st.PendingDebianUpdates)
	}
	if st.Available != nil {
		t.Fatal("GET without fetch should not hit the index")
	}
}

func isChecksumErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "checksum") || strings.Contains(err.Error(), ErrChecksumMismatch.Error()))
}

func TestSystemdInstaller_UsesArgvNotShell(t *testing.T) {
	ctx := context.Background()
	runner := &recordingRunner{}
	inst := SystemdInstaller{
		Runner:   runner,
		Hoservad: "/usr/bin/hoservad",
		StateDir: "/var/lib/hoserva",
	}
	pending := "/var/lib/hoserva/updates/pending"
	if err := inst.Install(ctx, pending); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %v", runner.calls)
	}
	got := runner.calls[0]
	if strings.Contains(got, "sh -c") || strings.Contains(got, "bash -c") {
		t.Fatalf("systemd-run used a shell: %s", got)
	}
	if !strings.Contains(got, "systemd-run") || !strings.Contains(got, "--apply-verified-update "+pending) {
		t.Fatalf("missing apply flag: %s", got)
	}
	if !strings.Contains(got, "--state-dir /var/lib/hoserva") {
		t.Fatalf("missing state-dir: %s", got)
	}
}

func asBlocked(err error, dest *ErrBlocked) bool {
	return errors.As(err, dest)
}

type recordingSnapshots struct {
	live string
}

func (r *recordingSnapshots) Snapshot(ctx context.Context, dbPath, dir string) (string, error) {
	return r.live, nil
}

func (r *recordingSnapshots) RestoreSnapshot(ctx context.Context, liveDB, snapshotPath string) error {
	return fmt.Errorf("restore should run in the transient unit, not Engine.Rollback")
}

type recordingRunner struct {
	calls []string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "apt-get" {
		return []byte("Inst openssl [3.0.0] (3.0.1 Debian:12/stable [amd64])\n"), nil
	}
	if name == "dpkg-query" {
		return []byte("2.40.2-5\n"), nil
	}
	return nil, nil
}

func TestCheck_NotifiesUpdateAvailableNotFailed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []byte("deb"), "")
	e, _, notes, _, _, _ := testEngine(t, f)
	e.Current = "0.1.0"

	_, err := e.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(notes.Events) != 1 || notes.Events[0] != notify.EventHoservaUpdateAvailable {
		t.Fatalf("notify events = %v, want [%s]", notes.Events, notify.EventHoservaUpdateAvailable)
	}
}
