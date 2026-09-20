package update

import (
	"context"
	"crypto/ed25519"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// DefaultIndexURL is the permanent signed release-index URL (Q66, Q67).
// hoservad's update check reads only this file, never the GitHub API.
const DefaultIndexURL = "https://hoserva.dev/releases/index.json"

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta"

	// Tested floors from Q7 / doc 08 §8. The updates page warns, not
	// blocks, when an installed package is below this.
	MergerfsFloor = "2.40.2"
	SnapraidFloor = "12.4"
)

// Channel is the release channel the index is read for.
type Channel string

// Fetcher retrieves a URL's body. Production uses HTTPFetcher; tests
// inject a map of URL → bytes so nothing on the host talks to the
// network. Get must not follow a redirect onto api.github.com.
type Fetcher interface {
	Get(ctx context.Context, url string) ([]byte, error)
}

// Installer starts the verified package install (and optional snapshot
// restore) in a transient systemd unit. Tests inject a FakeInstaller
// that records calls and never execs.
type Installer interface {
	Install(ctx context.Context, pendingDir string) error
}

// Host reports Debian package state and performs a reboot. Tests inject
// a FakeHost. Reboot is only ever called from Engine.Reboot.
type Host interface {
	PendingUpdates(ctx context.Context) ([]PendingUpdate, error)
	RebootRequired() bool
	PackageVersion(ctx context.Context, name string) (string, error)
	Reboot(ctx context.Context) error
}

// JobGuard is the scheduler's storage-class check. *job.Scheduler
// satisfies this.
type JobGuard interface {
	BlockingStorageJob() *job.Job
	WaitForStorageJobs(ctx context.Context) error
}

// Shutdown is Q70's array stop sequence. *job.ArraySequence.Stop
// satisfies this. Nil on Engine means no array is configured yet.
type Shutdown interface {
	Stop(ctx context.Context) error
}

// ConfigBackup is the config archive run automatically before a
// self-update (doc 10 §1). *backup.Service.Run satisfies this.
type ConfigBackup interface {
	Run(ctx context.Context) error
}

// SnapshotStore snapshots the live database at Apply (D16, Q67).
// RestoreSnapshot is the transient unit's job, not Engine.Rollback.
type SnapshotStore interface {
	Snapshot(ctx context.Context, dbPath, dir string) (string, error)
	RestoreSnapshot(ctx context.Context, liveDB, snapshotPath string) error
}

// Notifier raises in-app and routed alerts. *notify.Service.Publish
// satisfies this.
type Notifier interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// Settings persists channel, check-enabled and the previous version
// rollback would restore.
type Settings interface {
	Get(ctx context.Context) (SettingsRow, error)
	SetChannelAndCheck(ctx context.Context, channel Channel, checkEnabled bool) error
	SetPreviousVersion(ctx context.Context, version string) error
}

// SettingsRow is the persisted update-settings subset of schema_info.
type SettingsRow struct {
	Channel         Channel
	CheckEnabled    bool
	PreviousVersion string
}

// PendingUpdate is one Debian package with a candidate newer than the
// installed version, already in the apt cache — never the result of an
// apt update this process ran.
type PendingUpdate struct {
	Name             string
	InstalledVersion string
	CandidateVersion string
}

// DependencyStatus is mergerfs/SnapRAID against Q7's tested floor.
type DependencyStatus struct {
	Name             string
	InstalledVersion string
	TestedFloor      string
	InRange          bool
}

// Release is one entry from the signed release index.
type Release struct {
	Tag       string           `json:"tag"`
	Version   string           `json:"version"`
	Channel   Channel          `json:"channel"`
	Changelog string           `json:"changelog,omitempty"`
	Assets    map[string]Asset `json:"assets"`
}

// Asset is one architecture's .deb as named by the index.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Index is https://hoserva.dev/releases/index.json.
type Index struct {
	Channels map[Channel][]Release `json:"channels"`
}

// Status is the API-facing view of update state.
type Status struct {
	CurrentVersion       string
	Available            *Release
	Channel              Channel
	CheckEnabled         bool
	PreviousVersion      string
	RebootRequired       bool
	PendingDebianUpdates []PendingUpdate
	Dependencies         []DependencyStatus
	BlockingJob          *job.Job
}

// PublicKey is an Ed25519 public key that verifies SHA256SUMS.sig.
type PublicKey = ed25519.PublicKey
