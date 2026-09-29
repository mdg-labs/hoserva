package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	dockercontainer "github.com/moby/moby/api/types/container"
	dockerevents "github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"
)

// lifecycleTimeout bounds a start, stop, restart or remove: a stop alone
// waits up to the Engine's ten second grace period before it kills.
const lifecycleTimeout = 2 * time.Minute

func mapEngineErr(err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return wrapEngineErr(err)
}

// resolve finds the container by exact ID or name, like Inspect, so an
// action never lands on a container the caller only named by a prefix.
func (c *EngineClient) resolve(ctx context.Context, id string) (Container, error) {
	return c.Inspect(ctx, id)
}

func (c *EngineClient) Start(ctx context.Context, id string) error {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	_, err = c.cli.ContainerStart(ctx, ct.ID, dockerclient.ContainerStartOptions{})
	return mapEngineErr(err)
}

func (c *EngineClient) Stop(ctx context.Context, id string) error {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	_, err = c.cli.ContainerStop(ctx, ct.ID, dockerclient.ContainerStopOptions{})
	return mapEngineErr(err)
}

func (c *EngineClient) Restart(ctx context.Context, id string) error {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	_, err = c.cli.ContainerRestart(ctx, ct.ID, dockerclient.ContainerRestartOptions{})
	return mapEngineErr(err)
}

func (c *EngineClient) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return err
	}
	switch ct.State {
	case "created", "exited", "dead":
	default:
		return fmt.Errorf("%w (state %s)", ErrRunning, ct.State)
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	_, err = c.cli.ContainerRemove(ctx, ct.ID, dockerclient.ContainerRemoveOptions{RemoveVolumes: opts.Volumes})
	return mapEngineErr(err)
}

func (c *EngineClient) Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error) {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := c.inspectEngine(ctx, ct.ID)
	if err != nil {
		return nil, err
	}
	tail := "all"
	if opts.Tail >= 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	rc, err := c.cli.ContainerLogs(ctx, ct.ID, dockerclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       tail,
	})
	if err != nil {
		return nil, mapEngineErr(err)
	}
	if info.Config != nil && info.Config.Tty {
		return rc, nil
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = rc.Close()
		_ = pw.CloseWithError(err)
	}()
	return &demuxedLogs{PipeReader: pr, upstream: rc}, nil
}

// demuxedLogs is a container's log stream with the Engine's stdout/stderr
// framing removed. Closing it also closes the Engine connection, which is
// what ends the goroutine copying from it.
type demuxedLogs struct {
	*io.PipeReader
	upstream io.Closer
}

func (d *demuxedLogs) Close() error {
	_ = d.upstream.Close()
	return d.PipeReader.Close()
}

func (c *EngineClient) inspectEngine(ctx context.Context, id string) (dockercontainer.InspectResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	res, err := c.cli.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
	if err != nil {
		return dockercontainer.InspectResponse{}, mapEngineErr(err)
	}
	return res.Container, nil
}

func (c *EngineClient) Stats(ctx context.Context, id string) (Stats, error) {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return Stats{}, err
	}
	if ct.State != "running" {
		return Stats{}, fmt.Errorf("%w (state %s)", ErrNotRunning, ct.State)
	}
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	resp, err := c.cli.ContainerStats(ctx, ct.ID, dockerclient.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return Stats{}, mapEngineErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw dockercontainer.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Stats{}, fmt.Errorf("container: decoding stats: %w", err)
	}
	return statsFromEngine(raw), nil
}

// statsFromEngine reduces one Engine stats sample to Stats the way the
// docker CLI does: CPU as the share of the host's CPU time used between
// the sample and the one before it, times the CPUs online; memory without
// reclaimable page cache.
func statsFromEngine(s dockercontainer.StatsResponse) Stats {
	out := Stats{At: s.Read.UTC()}

	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && systemDelta > 0 && cpus > 0 {
		out.CPUPercent = cpuDelta / systemDelta * cpus * 100
	}

	mem := s.MemoryStats.Usage
	if v, ok := s.MemoryStats.Stats["total_inactive_file"]; ok && v < mem {
		mem -= v
	} else if v, ok := s.MemoryStats.Stats["inactive_file"]; ok && v < mem {
		mem -= v
	}
	out.MemoryBytes = mem
	out.MemoryLimitBytes = s.MemoryStats.Limit

	for _, n := range s.Networks {
		out.NetworkRxBytes += n.RxBytes
		out.NetworkTxBytes += n.TxBytes
	}
	for _, e := range s.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			out.BlockReadBytes += e.Value
		case "write":
			out.BlockWriteBytes += e.Value
		}
	}
	return out
}

// healthFromStatus reads the health check result out of the Engine's
// human-readable container status, the same text `docker ps` prints:
// "Up 3 hours (healthy)", "Up 3 minutes (unhealthy)",
// "Up 10 seconds (health: starting)". The container listing carries no
// structured health field.
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return HealthUnhealthy
	case strings.Contains(status, "(healthy)"):
		return HealthHealthy
	case strings.Contains(status, "(health: starting)"):
		return HealthStarting
	default:
		return HealthNone
	}
}

func (c *EngineClient) Watch(ctx context.Context, fn func(StateChange)) error {
	events := c.cli.Events(ctx, dockerclient.EventsListOptions{
		Filters: dockerclient.Filters{}.Add("type", string(dockerevents.ContainerEventType)),
	})
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-events.Err:
			if err == nil {
				return nil
			}
			return wrapEngineErr(err)
		case m, open := <-events.Messages:
			if !open {
				return nil
			}
			if sc, ok := changeFromEvent(m); ok {
				fn(sc)
			}
		}
	}
}

// changeFromEvent maps one Engine container event to a StateChange. Events
// that do not change a container's state or health (attach, exec, resize,
// rename) report false.
func changeFromEvent(m dockerevents.Message) (StateChange, bool) {
	if m.Type != dockerevents.ContainerEventType {
		return StateChange{}, false
	}
	at := time.Unix(m.Time, 0).UTC()
	if m.TimeNano != 0 {
		at = time.Unix(0, m.TimeNano).UTC()
	}
	sc := StateChange{ID: m.Actor.ID, Name: m.Actor.Attributes["name"], At: at}
	action := string(m.Action)
	switch {
	case action == string(dockerevents.ActionStart), action == string(dockerevents.ActionUnPause), action == string(dockerevents.ActionRestart):
		sc.State = "running"
	case action == string(dockerevents.ActionDie):
		sc.State = "exited"
	case action == string(dockerevents.ActionPause):
		sc.State = "paused"
	case strings.HasPrefix(action, string(dockerevents.ActionHealthStatus)):
		sc.State = "running"
		switch strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(action, string(dockerevents.ActionHealthStatus)), ":")) {
		case "healthy":
			sc.Health = HealthHealthy
		case "unhealthy":
			sc.Health = HealthUnhealthy
		case "starting", "running":
			sc.Health = HealthStarting
		default:
			return StateChange{}, false
		}
	default:
		return StateChange{}, false
	}
	return sc, true
}
