package update

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// MapFetcher serves Get from an in-memory map. Unknown URLs error.
type MapFetcher struct {
	Bodies map[string][]byte
	Hits   []string
}

func (f *MapFetcher) Get(ctx context.Context, url string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Hits = append(f.Hits, url)
	body, ok := f.Bodies[url]
	if !ok {
		return nil, fmt.Errorf("update: no fixture for %s", url)
	}
	return bytes.Clone(body), nil
}

// FakeInstaller records Install calls and never execs.
type FakeInstaller struct {
	mu      sync.Mutex
	Calls   []string
	Err     error
	Reboots int
}

func (f *FakeInstaller) Install(ctx context.Context, pendingDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, pendingDir)
	return f.Err
}

// FakeHost is a scriptable Host.
type FakeHost struct {
	Pending      []PendingUpdate
	PendingCalls int
	NeedReboot   bool
	Versions     map[string]string
	RebootCalls  int
	RebootErr    error
}

func (h *FakeHost) PendingUpdates(ctx context.Context) ([]PendingUpdate, error) {
	h.PendingCalls++
	return h.Pending, nil
}

func (h *FakeHost) RebootRequired() bool { return h.NeedReboot }

func (h *FakeHost) PackageVersion(ctx context.Context, name string) (string, error) {
	if h.Versions == nil {
		return "", fmt.Errorf("update: no version for %s", name)
	}
	v, ok := h.Versions[name]
	if !ok {
		return "", fmt.Errorf("update: no version for %s", name)
	}
	return v, nil
}

func (h *FakeHost) Reboot(ctx context.Context) error {
	h.RebootCalls++
	return h.RebootErr
}

// FakeJobs is a scriptable JobGuard.
type FakeJobs struct {
	Blocking *job.Job
	WaitErr  error
	WaitN    int
}

func (j *FakeJobs) BlockingStorageJob() *job.Job { return j.Blocking }

func (j *FakeJobs) WaitForStorageJobs(ctx context.Context) error {
	j.WaitN++
	return j.WaitErr
}

// FakeShutdown records Stop calls.
type FakeShutdown struct {
	Stops int
	Err   error
}

func (s *FakeShutdown) Stop(ctx context.Context) error {
	s.Stops++
	return s.Err
}

// FakeBackup records Run calls.
type FakeBackup struct {
	Runs int
	Err  error
}

func (b *FakeBackup) Run(ctx context.Context) error {
	b.Runs++
	return b.Err
}

// FakeNotifier records Publish calls.
type FakeNotifier struct {
	Events []notify.EventType
	Titles []string
}

func (n *FakeNotifier) Publish(ctx context.Context, event notify.EventType, title, message string) error {
	n.Events = append(n.Events, event)
	n.Titles = append(n.Titles, title+"\n"+message)
	return nil
}

// MemorySettings is an in-memory Settings.
type MemorySettings struct {
	Row SettingsRow
}

func (s *MemorySettings) Get(ctx context.Context) (SettingsRow, error) {
	row := s.Row
	if row.Channel == "" {
		row.Channel = ChannelStable
	}
	return row, nil
}

func (s *MemorySettings) SetChannelAndCheck(ctx context.Context, channel Channel, checkEnabled bool) error {
	s.Row.Channel = channel
	s.Row.CheckEnabled = checkEnabled
	return nil
}

func (s *MemorySettings) SetPreviousVersion(ctx context.Context, version string) error {
	s.Row.PreviousVersion = version
	return nil
}

// DefaultMemorySettings is a fresh install's update settings.
func DefaultMemorySettings() *MemorySettings {
	return &MemorySettings{Row: SettingsRow{Channel: ChannelStable, CheckEnabled: true}}
}

func (n *FakeNotifier) notifiedFailure() bool {
	for _, event := range n.Events {
		if event == notify.EventHoservaUpdateFailed {
			return true
		}
	}
	return false
}
