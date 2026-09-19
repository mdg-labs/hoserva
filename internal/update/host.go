package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// HTTPFetcher is the production Fetcher. It never talks to api.github.com.
type HTTPFetcher struct {
	Client *http.Client
}

func (f HTTPFetcher) Get(ctx context.Context, url string) ([]byte, error) {
	if strings.Contains(url, "api.github.com") {
		return nil, fmt.Errorf("%w: %s", ErrIndexURL, url)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: GET %s: %s", url, resp.Status)
	}
	const max = 256 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("update: reading %s: %w", url, err)
	}
	if len(body) > max {
		return nil, fmt.Errorf("update: GET %s: body too large", url)
	}
	return body, nil
}

// DebianHost reads apt/dpkg state and reboots through Runner (argv, never
// a shell). Tests never construct this; they use FakeHost.
type DebianHost struct {
	Runner             disk.Runner
	RebootRequiredPath string
}

func (h DebianHost) rebootRequiredPath() string {
	if h.RebootRequiredPath != "" {
		return h.RebootRequiredPath
	}
	return "/run/reboot-required"
}

func (h DebianHost) RebootRequired() bool {
	_, err := os.Stat(h.rebootRequiredPath())
	return err == nil
}

func (h DebianHost) PackageVersion(ctx context.Context, name string) (string, error) {
	out, err := h.Runner.Run(ctx, "dpkg-query", "-W", "-f", "${Version}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (h DebianHost) PendingUpdates(ctx context.Context) ([]PendingUpdate, error) {
	out, err := h.Runner.Run(ctx, "apt-get", "-s", "-o", "Debug::NoLocking=true", "upgrade")
	if err != nil {
		return nil, err
	}
	return parseSimulatedUpgrade(string(out)), nil
}

func (h DebianHost) Reboot(ctx context.Context) error {
	_, err := h.Runner.Run(ctx, "systemctl", "reboot")
	return err
}

func parseSimulatedUpgrade(out string) []PendingUpdate {
	var pending []PendingUpdate
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		name := fields[1]
		installed := strings.Trim(fields[2], "[]")
		candidate := strings.Trim(fields[3], "()")
		pending = append(pending, PendingUpdate{
			Name:             name,
			InstalledVersion: installed,
			CandidateVersion: candidate,
		})
	}
	return pending
}

// SystemdInstaller starts hoservad --apply-verified-update against the
// pending directory via systemd-run, so the daemon can restart itself
// (Q67). Conflicts=hoserva.service stops the running daemon first so a
// snapshot restore can replace the live database.
type SystemdInstaller struct {
	Runner     disk.Runner
	Hoservad   string
	StateDir   string
	UnitPrefix string
}

func (i SystemdInstaller) Install(ctx context.Context, pendingDir string) error {
	bin := i.Hoservad
	if bin == "" {
		var err error
		bin, err = os.Executable()
		if err != nil {
			bin = "/usr/bin/hoservad"
		}
	}
	stateDir := i.StateDir
	if stateDir == "" {
		stateDir = filepath.Dir(filepath.Dir(pendingDir))
	}
	unit := i.UnitPrefix
	if unit == "" {
		unit = "hoserva-update"
	}
	_, err := i.Runner.Run(ctx, "systemd-run",
		"--collect",
		"--unit="+unit,
		"--property=Type=oneshot",
		"--property=Conflicts=hoserva.service",
		"--property=After=hoserva.service",
		bin, "--state-dir", stateDir, "--apply-verified-update", pendingDir,
	)
	return err
}
