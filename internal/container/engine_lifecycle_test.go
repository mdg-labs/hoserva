package container

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	dockercontainer "github.com/moby/moby/api/types/container"
	dockerevents "github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"
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
	opts   dockerclient.ContainerLogsOptions
	closed bool
}

func (e *logsEngine) ContainerList(context.Context, dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: []dockercontainer.Summary{{ID: "c1", Names: []string{"/jellyfin"}, State: "running"}}}, nil
}

func (e *logsEngine) ContainerInspect(context.Context, string, dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	return dockerclient.ContainerInspectResult{Container: dockercontainer.InspectResponse{Config: &dockercontainer.Config{Tty: e.tty}}}, nil
}

func (e *logsEngine) ContainerLogs(_ context.Context, _ string, o dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
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

// writeFrame appends one Engine stream frame: a stream byte, three zero
// bytes and the payload length as a big-endian uint32, then the payload.
func writeFrame(buf *bytes.Buffer, stream stdcopy.StdType, payload string) {
	header := [8]byte{byte(stream)}
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	buf.Write(header[:])
	buf.WriteString(payload)
}

func TestLogs_StripsTheEnginesStreamFraming(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, stdcopy.Stdout, "hello\n")
	writeFrame(&buf, stdcopy.Stderr, "oops\n")
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
	removed *dockerclient.ContainerRemoveOptions
}

func (e *removeEngine) ContainerList(context.Context, dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: []dockercontainer.Summary{{ID: "c1", Names: []string{"/jellyfin"}, State: e.state}}}, nil
}

func (e *removeEngine) ContainerRemove(_ context.Context, _ string, o dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	e.removed = &o
	return dockerclient.ContainerRemoveResult{}, nil
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

// statsEngine serves one stats sample.
type statsEngine struct {
	engineAPI
	body string
	opts dockerclient.ContainerStatsOptions
}

func (e *statsEngine) ContainerList(context.Context, dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: []dockercontainer.Summary{{ID: "c1", Names: []string{"/jellyfin"}, State: "running"}}}, nil
}

func (e *statsEngine) ContainerStats(_ context.Context, _ string, o dockerclient.ContainerStatsOptions) (dockerclient.ContainerStatsResult, error) {
	e.opts = o
	return dockerclient.ContainerStatsResult{Body: io.NopCloser(strings.NewReader(e.body))}, nil
}

// The CPU share is the delta against the previous sample, which the Engine
// only includes when asked for it: without IncludePreviousSample every
// reading would report 0% CPU.
func TestEngineStats_AsksForThePreviousSampleAndReducesIt(t *testing.T) {
	eng := &statsEngine{body: `{"read":"2026-09-28T12:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":300},"system_cpu_usage":2000,"online_cpus":4},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},"memory_stats":{"usage":1000,"limit":8000}}`}
	got, err := (&EngineClient{cli: eng}).Stats(context.Background(), "jellyfin")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if eng.opts.Stream || !eng.opts.IncludePreviousSample {
		t.Fatalf("stats options = %+v, want one sample that includes the previous one", eng.opts)
	}
	if math.Abs(got.CPUPercent-80) > 1e-9 || got.MemoryBytes != 1000 || got.MemoryLimitBytes != 8000 {
		t.Fatalf("Stats = %+v, want 80%% CPU and the memory figures", got)
	}
}

// eventsEngine feeds Watch one canned event stream.
type eventsEngine struct {
	engineAPI
	msgs chan dockerevents.Message
	errs chan error
	opts dockerclient.EventsListOptions
}

func (e *eventsEngine) Events(_ context.Context, o dockerclient.EventsListOptions) dockerclient.EventsResult {
	e.opts = o
	return dockerclient.EventsResult{Messages: e.msgs, Err: e.errs}
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
	if want := (dockerclient.Filters{"type": {"container": true}}); !reflect.DeepEqual(eng.opts.Filters, want) {
		t.Fatalf("event filters = %v, want only container events %v", eng.opts.Filters, want)
	}
}
