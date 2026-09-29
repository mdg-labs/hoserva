package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/pkg/stdcopy"
)

func TestHealthFromStatus(t *testing.T) {
	for status, want := range map[string]string{
		"Up 3 hours (healthy)":             HealthHealthy,
		"Up 3 minutes (unhealthy)":         HealthUnhealthy,
		"Up 10 seconds (health: starting)": HealthStarting,
		"Up 3 hours":                       HealthNone,
		"Exited (0) 2 days ago":            HealthNone,
		"":                                 HealthNone,
	} {
		if got := healthFromStatus(status); got != want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestContainerFromSummary_ReportsHealth(t *testing.T) {
	c := containerFromSummary(dockercontainer.Summary{ID: "c1", Names: []string{"/jf"}, State: "running", Status: "Up 2 minutes (unhealthy)"})
	if c.Health != HealthUnhealthy {
		t.Fatalf("Health = %q, want unhealthy", c.Health)
	}
}

func TestChangeFromEvent(t *testing.T) {
	msg := func(action string) dockerevents.Message {
		return dockerevents.Message{
			Type:     dockerevents.ContainerEventType,
			Action:   dockerevents.Action(action),
			Actor:    dockerevents.Actor{ID: "c1", Attributes: map[string]string{"name": "jellyfin"}},
			TimeNano: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).UnixNano(),
		}
	}
	for _, tc := range []struct {
		action, state, health string
		ok                    bool
	}{
		{"start", "running", "", true},
		{"unpause", "running", "", true},
		{"restart", "running", "", true},
		{"die", "exited", "", true},
		{"pause", "paused", "", true},
		{"health_status: unhealthy", "running", HealthUnhealthy, true},
		{"health_status: healthy", "running", HealthHealthy, true},
		{"health_status: starting", "running", HealthStarting, true},
		{"health_status: running", "running", HealthStarting, true},
		{"health_status: some free-form output", "", "", false},
		{"kill", "", "", false},
		{"exec_create: sh", "", "", false},
		{"attach", "", "", false},
	} {
		sc, ok := changeFromEvent(msg(tc.action))
		if ok != tc.ok {
			t.Errorf("%q: ok = %v, want %v", tc.action, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if sc.State != tc.state || sc.Health != tc.health || sc.ID != "c1" || sc.Name != "jellyfin" || sc.At.Year() != 2026 {
			t.Errorf("%q: got %+v, want state %q health %q", tc.action, sc, tc.state, tc.health)
		}
	}
	if _, ok := changeFromEvent(dockerevents.Message{Type: dockerevents.NetworkEventType, Action: "connect"}); ok {
		t.Error("a network event was mapped to a container state change")
	}
}

func TestStatsFromEngine(t *testing.T) {
	var s dockercontainer.StatsResponse
	s.Read = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	s.CPUStats.CPUUsage.TotalUsage = 300
	s.PreCPUStats.CPUUsage.TotalUsage = 100
	s.CPUStats.SystemUsage = 2000
	s.PreCPUStats.SystemUsage = 1000
	s.CPUStats.OnlineCPUs = 4
	s.MemoryStats.Usage = 1000
	s.MemoryStats.Limit = 8000
	s.MemoryStats.Stats = map[string]uint64{"inactive_file": 200}
	s.Networks = map[string]dockercontainer.NetworkStats{
		"eth0": {RxBytes: 10, TxBytes: 20},
		"eth1": {RxBytes: 1, TxBytes: 2},
	}
	s.BlkioStats.IoServiceBytesRecursive = []dockercontainer.BlkioStatEntry{
		{Op: "Read", Value: 5}, {Op: "Write", Value: 7}, {Op: "read", Value: 1}, {Op: "Total", Value: 99},
	}

	got := statsFromEngine(s)

	if math.Abs(got.CPUPercent-80) > 1e-9 {
		t.Errorf("CPUPercent = %v, want 80 (200/1000 of host CPU time on 4 CPUs)", got.CPUPercent)
	}
	if got.MemoryBytes != 800 || got.MemoryLimitBytes != 8000 {
		t.Errorf("memory = %d/%d, want 800/8000 (page cache excluded)", got.MemoryBytes, got.MemoryLimitBytes)
	}
	if got.NetworkRxBytes != 11 || got.NetworkTxBytes != 22 {
		t.Errorf("network = %d/%d, want 11/22", got.NetworkRxBytes, got.NetworkTxBytes)
	}
	if got.BlockReadBytes != 6 || got.BlockWriteBytes != 7 {
		t.Errorf("block io = %d/%d, want 6/7", got.BlockReadBytes, got.BlockWriteBytes)
	}
	if !got.At.Equal(s.Read) {
		t.Errorf("At = %v, want %v", got.At, s.Read)
	}
}

func TestStatsFromEngine_FirstSampleHasNoCPUDelta(t *testing.T) {
	var s dockercontainer.StatsResponse
	s.CPUStats.CPUUsage.TotalUsage = 300
	s.CPUStats.SystemUsage = 2000
	if got := statsFromEngine(s); got.CPUPercent != 0 {
		t.Fatalf("CPUPercent = %v, want 0 when there is no previous sample", got.CPUPercent)
	}
}

// logsEngine serves one framed (non-TTY) log stream.
type logsEngine struct {
	engineAPI
	stream []byte
	tty    bool
	opts   dockercontainer.LogsOptions
	closed bool
}

func (e *logsEngine) ContainerList(context.Context, dockercontainer.ListOptions) ([]dockercontainer.Summary, error) {
	return []dockercontainer.Summary{{ID: "c1", Names: []string{"/jellyfin"}, State: "running"}}, nil
}

func (e *logsEngine) ContainerInspect(context.Context, string) (dockercontainer.InspectResponse, error) {
	return dockercontainer.InspectResponse{Config: &dockercontainer.Config{Tty: e.tty}}, nil
}

func (e *logsEngine) ContainerLogs(_ context.Context, _ string, o dockercontainer.LogsOptions) (io.ReadCloser, error) {
	e.opts = o
	return &trackedCloser{Reader: bytes.NewReader(e.stream), closed: &e.closed}, nil
}

type trackedCloser struct {
	io.Reader
	closed *bool
}

func (c *trackedCloser) Close() error {
	*c.closed = true
	return nil
}

func TestLogs_StripsTheEnginesStreamFraming(t *testing.T) {
	var buf bytes.Buffer
	w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	_, _ = w.Write([]byte("hello\n"))
	e := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
	_, _ = e.Write([]byte("oops\n"))
	eng := &logsEngine{stream: buf.Bytes()}

	rc, err := (&EngineClient{cli: eng}).Logs(context.Background(), "jellyfin", LogOptions{Tail: 50, Follow: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	_ = rc.Close()
	if string(got) != "hello\noops\n" {
		t.Fatalf("logs = %q, want the two lines without framing bytes", got)
	}
	if !eng.opts.Follow || eng.opts.Tail != "50" || !eng.opts.ShowStdout || !eng.opts.ShowStderr {
		t.Fatalf("options = %+v, want stdout and stderr, tail 50, follow", eng.opts)
	}
	if !eng.closed {
		t.Fatal("the Engine connection was not closed")
	}
}

func TestLogs_TTYOutputIsPassedThrough(t *testing.T) {
	eng := &logsEngine{stream: []byte("plain tty output\n"), tty: true}
	rc, err := (&EngineClient{cli: eng}).Logs(context.Background(), "jellyfin", LogOptions{Tail: -1})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "plain tty output\n" {
		t.Fatalf("logs = %q", got)
	}
	if eng.opts.Tail != "all" {
		t.Fatalf("Tail = %q, want all for a negative tail", eng.opts.Tail)
	}
}

func TestLogs_UnknownContainer(t *testing.T) {
	eng := &logsEngine{}
	if _, err := (&EngineClient{cli: eng}).Logs(context.Background(), "nope", LogOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// removeEngine proves Remove refuses a live container before it reaches the
// Engine at all.
type removeEngine struct {
	engineAPI
	state   dockercontainer.ContainerState
	removed *dockercontainer.RemoveOptions
}

func (e *removeEngine) ContainerList(context.Context, dockercontainer.ListOptions) ([]dockercontainer.Summary, error) {
	return []dockercontainer.Summary{{ID: "c1", Names: []string{"/jellyfin"}, State: e.state}}, nil
}

func (e *removeEngine) ContainerRemove(_ context.Context, _ string, o dockercontainer.RemoveOptions) error {
	e.removed = &o
	return nil
}

func TestEngineRemove_RefusesALiveContainerAndNeverForces(t *testing.T) {
	live := &removeEngine{state: "running"}
	if err := (&EngineClient{cli: live}).Remove(context.Background(), "jellyfin", RemoveOptions{}); !errors.Is(err, ErrRunning) {
		t.Fatalf("Remove error = %v, want ErrRunning", err)
	}
	if live.removed != nil {
		t.Fatal("a running container reached ContainerRemove")
	}

	stopped := &removeEngine{state: "exited"}
	if err := (&EngineClient{cli: stopped}).Remove(context.Background(), "jellyfin", RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if stopped.removed == nil || stopped.removed.Force || stopped.removed.RemoveVolumes {
		t.Fatalf("ContainerRemove options = %+v, want neither Force nor RemoveVolumes by default", stopped.removed)
	}
	withVolumes := &removeEngine{state: "exited"}
	if err := (&EngineClient{cli: withVolumes}).Remove(context.Background(), "jellyfin", RemoveOptions{Volumes: true}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !withVolumes.removed.RemoveVolumes || withVolumes.removed.Force {
		t.Fatalf("ContainerRemove options = %+v, want RemoveVolumes only when asked", withVolumes.removed)
	}
}

// eventsEngine feeds Watch one canned event stream.
type eventsEngine struct {
	engineAPI
	msgs chan dockerevents.Message
	errs chan error
}

func (e *eventsEngine) Events(context.Context, dockerevents.ListOptions) (<-chan dockerevents.Message, <-chan error) {
	return e.msgs, e.errs
}

func TestEngineWatch_DeliversEventsAndReportsStreamFailure(t *testing.T) {
	eng := &eventsEngine{msgs: make(chan dockerevents.Message), errs: make(chan error, 1)}
	go func() {
		eng.msgs <- dockerevents.Message{Type: dockerevents.ContainerEventType, Action: "attach", Actor: dockerevents.Actor{ID: "c1"}}
		eng.msgs <- dockerevents.Message{Type: dockerevents.ContainerEventType, Action: "die", Actor: dockerevents.Actor{ID: "c1", Attributes: map[string]string{"name": "jellyfin"}}}
		eng.errs <- errors.New("connection reset")
	}()

	var got []StateChange
	err := (&EngineClient{cli: eng}).Watch(context.Background(), func(sc StateChange) { got = append(got, sc) })
	if err == nil {
		t.Fatal("Watch returned nil for a failed event stream")
	}
	if len(got) != 1 || got[0].State != "exited" || got[0].Name != "jellyfin" {
		t.Fatalf("delivered %+v, want the one die event and not the attach event", got)
	}
}
