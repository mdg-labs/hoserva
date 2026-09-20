package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	pendingDebName      = "package.deb"
	pendingHashName     = "package.sha256"
	pendingSnapshotName = "restore-snapshot"
)

// Engine is the self-update coordinator. Every host-touching dependency
// is an interface so tests inject fakes.
type Engine struct {
	IndexURL    string
	Arch        string
	StateDir    string
	SnapshotDir string
	DBPath      string
	Current     string
	PublicKey   PublicKey
	Fetcher     Fetcher
	Installer   Installer
	Host        Host
	Jobs        JobGuard
	Shutdown    Shutdown
	Backup      ConfigBackup
	Snapshots   SnapshotStore
	Notify      Notifier
	Settings    Settings
}

func (e *Engine) indexURL() string {
	if e.IndexURL != "" {
		return e.IndexURL
	}
	return DefaultIndexURL
}

func (e *Engine) arch() string {
	if e.Arch != "" {
		return e.Arch
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		return runtime.GOARCH
	}
}

func (e *Engine) publicKey() PublicKey {
	if len(e.PublicKey) > 0 {
		return e.PublicKey
	}
	return EmbeddedPublicKey
}

func (e *Engine) pendingDir() string {
	return filepath.Join(e.StateDir, "updates", "pending")
}

// Status returns the current update view. It fetches the index only when
// fetchIndex is true (a user-initiated check, or GET while the periodic
// check is enabled).
func (e *Engine) Status(ctx context.Context, fetchIndex bool) (Status, error) {
	row, err := e.settings(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		CurrentVersion:  e.Current,
		Channel:         row.Channel,
		CheckEnabled:    row.CheckEnabled,
		PreviousVersion: row.PreviousVersion,
	}
	if e.Host != nil {
		st.RebootRequired = e.Host.RebootRequired()
		pending, err := e.Host.PendingUpdates(ctx)
		if err != nil {
			return Status{}, err
		}
		st.PendingDebianUpdates = pending
		st.Dependencies, err = e.dependencies(ctx)
		if err != nil {
			return Status{}, err
		}
	}
	if e.Jobs != nil {
		st.BlockingJob = e.Jobs.BlockingStorageJob()
	}
	if fetchIndex {
		avail, err := e.lookupAvailable(ctx, row.Channel)
		if err != nil {
			return Status{}, err
		}
		st.Available = avail
	}
	return st, nil
}

func (e *Engine) settings(ctx context.Context) (SettingsRow, error) {
	if e.Settings == nil {
		return SettingsRow{Channel: ChannelStable, CheckEnabled: true}, nil
	}
	row, err := e.Settings.Get(ctx)
	if err != nil {
		return SettingsRow{}, err
	}
	if row.Channel == "" {
		row.Channel = ChannelStable
	}
	return row, nil
}

func (e *Engine) dependencies(ctx context.Context) ([]DependencyStatus, error) {
	out := make([]DependencyStatus, 0, 2)
	for _, d := range []struct{ name, floor string }{
		{"mergerfs", MergerfsFloor},
		{"snapraid", SnapraidFloor},
	} {
		ver, err := e.Host.PackageVersion(ctx, d.name)
		if err != nil {
			ver = ""
		}
		out = append(out, DependencyStatus{
			Name:             d.name,
			InstalledVersion: ver,
			TestedFloor:      d.floor,
			InRange:          versionAtLeast(ver, d.floor),
		})
	}
	return out, nil
}

func (e *Engine) lookupAvailable(ctx context.Context, channel Channel) (*Release, error) {
	idx, err := e.fetchIndex(ctx)
	if err != nil {
		return nil, err
	}
	return idx.latest(channel, e.Current), nil
}

func (e *Engine) fetchIndex(ctx context.Context) (*Index, error) {
	if e.Fetcher == nil {
		return nil, fmt.Errorf("update: no fetcher configured")
	}
	url := e.indexURL()
	if err := e.allowURL(url); err != nil {
		return nil, err
	}
	body, err := e.Fetcher.Get(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("update: fetching release index: %w", err)
	}
	return parseIndex(body)
}

// allowURL refuses api.github.com and any URL that isn't the configured
// index or a GitHub Releases *download* (asset) URL. Hosts are compared
// after parsing, never as a substring. The GitHub API is never used (Q67).
func (e *Engine) allowURL(raw string) error {
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return err
	}
	if hostnameIs(u, "api.github.com") {
		return fmt.Errorf("%w: %s", ErrIndexURL, raw)
	}
	if raw == e.indexURL() {
		return nil
	}
	if hostnameIs(u, "github.com") && strings.Contains(u.Path, "/releases/download/") {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrIndexURL, raw)
}

// PersistedSettings returns the stored channel and check-enabled flags
// without talking to apt or the release index.
func (e *Engine) PersistedSettings(ctx context.Context) (SettingsRow, error) {
	return e.settings(ctx)
}

// SetSettings persists channel and check-enabled.
func (e *Engine) SetSettings(ctx context.Context, channel *Channel, checkEnabled *bool) (Status, error) {
	row, err := e.settings(ctx)
	if err != nil {
		return Status{}, err
	}
	if channel != nil {
		switch *channel {
		case ChannelStable, ChannelBeta:
			row.Channel = *channel
		default:
			return Status{}, fmt.Errorf("update: unknown channel %q", *channel)
		}
	}
	if checkEnabled != nil {
		row.CheckEnabled = *checkEnabled
	}
	if e.Settings != nil {
		if err := e.Settings.SetChannelAndCheck(ctx, row.Channel, row.CheckEnabled); err != nil {
			return Status{}, err
		}
	}
	return e.Status(ctx, row.CheckEnabled)
}

// Check fetches the index even when the periodic check is disabled.
func (e *Engine) Check(ctx context.Context) (Status, error) {
	st, err := e.Status(ctx, true)
	if err != nil {
		return Status{}, err
	}
	if st.Available != nil && e.Notify != nil {
		_ = e.Notify.Publish(ctx, notify.EventHoservaUpdateAvailable,
			"Hoserva update available",
			fmt.Sprintf("Version %s is available.", st.Available.Version))
	}
	return st, nil
}

// Apply downloads, verifies and queues install of the available release.
func (e *Engine) Apply(ctx context.Context) error {
	if err := e.refuseIfBlocked(); err != nil {
		return err
	}
	row, err := e.settings(ctx)
	if err != nil {
		return err
	}
	rel, err := e.lookupAvailable(ctx, row.Channel)
	if err != nil {
		return err
	}
	if rel == nil {
		return ErrNotAvailable
	}
	if err := e.backup(ctx); err != nil {
		return err
	}
	if err := e.downloadAndVerify(ctx, rel); err != nil {
		e.notifyFailed(ctx, "Hoserva update was not installed", err.Error())
		return err
	}
	if err := e.recordLiveSnapshot(ctx); err != nil {
		return err
	}
	if e.Settings != nil && e.Current != "" {
		if err := e.Settings.SetPreviousVersion(ctx, e.Current); err != nil {
			return fmt.Errorf("update: recording previous version: %w", err)
		}
	}
	return e.installPending(ctx)
}

// Rollback downloads the previous release, verifies it, restores the
// live-database snapshot recorded at Apply, and queues install.
func (e *Engine) Rollback(ctx context.Context) error {
	if err := e.refuseIfBlocked(); err != nil {
		return err
	}
	row, err := e.settings(ctx)
	if err != nil {
		return err
	}
	if row.PreviousVersion == "" {
		return ErrNoPrevious
	}
	idx, err := e.fetchIndex(ctx)
	if err != nil {
		return err
	}
	rel := idx.findVersion(row.PreviousVersion)
	if rel == nil {
		return fmt.Errorf("update: previous version %s is not in the release index", row.PreviousVersion)
	}
	if err := e.backup(ctx); err != nil {
		return err
	}
	if err := e.downloadAndVerify(ctx, rel); err != nil {
		e.notifyFailed(ctx, "Hoserva rollback was not installed", err.Error())
		return err
	}
	if err := e.writeSnapshotMarker(ctx); err != nil {
		return err
	}
	return e.installPending(ctx)
}

func (e *Engine) writeSnapshotMarker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := e.snapshotDir()
	if dir == "" {
		return fmt.Errorf("update: no snapshot directory configured")
	}
	path, err := store.ReadRollbackTarget(dir)
	if err != nil {
		return fmt.Errorf("update: locating apply-time database snapshot: %w", err)
	}
	pending := e.pendingDir()
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(pending, pendingSnapshotName)
	return os.WriteFile(marker, []byte(path+"\n"), 0o600)
}

func (e *Engine) recordLiveSnapshot(ctx context.Context) error {
	dir := e.snapshotDir()
	if dir == "" {
		return fmt.Errorf("update: no snapshot directory configured")
	}
	var path string
	var err error
	if e.Snapshots != nil {
		path, err = e.Snapshots.Snapshot(ctx, e.DBPath, dir)
	} else {
		if e.DBPath == "" {
			return fmt.Errorf("update: no database path configured")
		}
		path, err = store.SnapshotLive(ctx, e.DBPath, dir)
	}
	if err != nil {
		return fmt.Errorf("update: snapshotting live database before upgrade: %w", err)
	}
	if err := store.WriteRollbackTarget(dir, path); err != nil {
		return fmt.Errorf("update: recording rollback snapshot: %w", err)
	}
	return nil
}

func (e *Engine) snapshotDir() string {
	if e.SnapshotDir != "" {
		return e.SnapshotDir
	}
	if e.StateDir == "" {
		return ""
	}
	return filepath.Join(e.StateDir, "backups", "pre-migration")
}

// Reboot waits for storage jobs, runs the Q70 sequence, then reboots.
// Nothing else on Engine calls Host.Reboot.
func (e *Engine) Reboot(ctx context.Context) error {
	if e.Jobs != nil {
		if err := e.Jobs.WaitForStorageJobs(ctx); err != nil {
			return err
		}
	}
	if e.Shutdown != nil {
		if err := e.Shutdown.Stop(ctx); err != nil {
			return fmt.Errorf("update: clean shutdown before reboot: %w", err)
		}
	}
	if e.Host == nil {
		return fmt.Errorf("update: no host reboot configured")
	}
	return e.Host.Reboot(ctx)
}

func (e *Engine) refuseIfBlocked() error {
	if e.Jobs == nil {
		return nil
	}
	if j := e.Jobs.BlockingStorageJob(); j != nil {
		return ErrBlocked{Job: j}
	}
	return nil
}

func (e *Engine) backup(ctx context.Context) error {
	if e.Backup == nil {
		return nil
	}
	if err := e.Backup.Run(ctx); err != nil {
		return fmt.Errorf("update: config backup before install: %w", err)
	}
	return nil
}

func (e *Engine) downloadAndVerify(ctx context.Context, rel *Release) error {
	asset, err := assetForArch(rel, e.arch())
	if err != nil {
		return err
	}
	if err := e.allowURL(asset.URL); err != nil {
		return err
	}
	sumsURL, sigURL, err := sumsURLs(asset.URL)
	if err != nil {
		return err
	}
	if err := e.allowURL(sumsURL); err != nil {
		return err
	}
	if err := e.allowURL(sigURL); err != nil {
		return err
	}

	sums, err := e.Fetcher.Get(ctx, sumsURL)
	if err != nil {
		return fmt.Errorf("update: fetching SHA256SUMS: %w", err)
	}
	sig, err := e.Fetcher.Get(ctx, sigURL)
	if err != nil {
		return fmt.Errorf("update: fetching SHA256SUMS.sig: %w", err)
	}
	if err := VerifySHA256SUMS(e.publicKey(), sums, sig); err != nil {
		return err
	}
	filename := pathBase(asset.URL)
	want, err := ChecksumFor(sums, filename)
	if err != nil {
		return err
	}
	if !checksumsMatch(want, asset.SHA256) {
		return fmt.Errorf("%w: index sha256 does not match signed SHA256SUMS", ErrChecksumMismatch)
	}

	deb, err := e.Fetcher.Get(ctx, asset.URL)
	if err != nil {
		return fmt.Errorf("update: downloading package: %w", err)
	}
	got := sha256Hex(deb)
	if !checksumsMatch(got, want) {
		return fmt.Errorf("%w: downloaded %s has checksum %s, signed SHA256SUMS has %s", ErrChecksumMismatch, filename, got, want)
	}

	pending := e.pendingDir()
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return fmt.Errorf("update: creating pending directory: %w", err)
	}
	_ = os.Remove(filepath.Join(pending, pendingSnapshotName))
	debPath := filepath.Join(pending, pendingDebName)
	if err := os.WriteFile(debPath, deb, 0o600); err != nil {
		return fmt.Errorf("update: writing pending package: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pending, pendingHashName), []byte(want+"\n"), 0o600); err != nil {
		return fmt.Errorf("update: writing pending checksum: %w", err)
	}
	return nil
}

func (e *Engine) installPending(ctx context.Context) error {
	if e.Installer == nil {
		return fmt.Errorf("update: no installer configured")
	}
	return e.Installer.Install(ctx, e.pendingDir())
}

func (e *Engine) notifyFailed(ctx context.Context, title, message string) {
	if e.Notify == nil {
		return
	}
	_ = e.Notify.Publish(ctx, notify.EventHoservaUpdateAvailable, title, message)
}

func pathBase(url string) string {
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return url
	}
	return url[i+1:]
}

// versionAtLeast is a Debian-ish numeric-prefix compare: 2.40.2-5 is at
// least 2.40.2. An empty installed version is not in range.
func versionAtLeast(installed, floor string) bool {
	if installed == "" {
		return false
	}
	return compareDebianPrefix(installed, floor) >= 0
}

func compareDebianPrefix(a, b string) int {
	as := versionNums(a)
	bs := versionNums(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(as) {
			ai = as[i]
		}
		if i < len(bs) {
			bi = bs[i]
		}
		if ai != bi {
			return ai - bi
		}
	}
	return 0
}

func versionNums(s string) []int {
	s = strings.TrimPrefix(s, "v")
	var out []int
	n := 0
	in := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			in = true
			continue
		}
		if in {
			out = append(out, n)
			n = 0
			in = false
		}
		if r == '-' || r == '~' {
			break
		}
	}
	if in {
		out = append(out, n)
	}
	return out
}
