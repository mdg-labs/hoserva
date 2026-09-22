package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

// upsTestdataDir is package-owned (internal/config/testdata/ups),
// mirroring poolMountTestdataDir's own precedent (pool_test.go): the
// shared testdata/configs/ root is walked unconditionally by the
// samba/nfs/snapraid golden tests for their own state.json shape, so a
// state.json in UPSState's shape collides with it there.
const upsTestdataDir = "testdata/ups"

func loadUPSState(t *testing.T, dir string) UPSState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(upsTestdataDir, dir, "state.json"))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	var state UPSState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("parsing state.json: %v", err)
	}
	return state
}

// TestRenderUPS walks every case under testdata/ups and compares each
// render function's output against its checked-in golden file (CLAUDE.md:
// "golden files change only deliberately"). The USB case additionally
// covers ups.conf and upsd.users, which only apply to a USB-attached UPS
// (RenderUPSConf's own doc comment).
func TestRenderUPS(t *testing.T) {
	cases, err := os.ReadDir(upsTestdataDir)
	if err != nil {
		t.Fatalf("reading %s: %v", upsTestdataDir, err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases under testdata/ups")
	}

	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		name := c.Name()
		t.Run(name, func(t *testing.T) {
			state := loadUPSState(t, name)
			dir := filepath.Join(upsTestdataDir, name)

			golden.Compare(t, filepath.Join(dir, "nut.conf.golden"), []byte(RenderNUTConf(state)))
			golden.Compare(t, filepath.Join(dir, "upsmon.conf.golden"), []byte(RenderUPSMonConf(state)))

			if state.Connection != UPSConnectionUSB {
				return
			}
			golden.Compare(t, filepath.Join(dir, "ups.conf.golden"), []byte(RenderUPSConf(state)))
			golden.Compare(t, filepath.Join(dir, "upsd.users.golden"), []byte(RenderUPSDUsers(state)))
		})
	}
}

// TestRenderUPSMonConf_NetworkOmitsLocalOnlyOverrides proves
// LowBatteryPercent/RuntimeSeconds never leak into upsmon.conf itself —
// they only ever render into the local ups.conf driver stanza (USB mode),
// per RenderUPSConf's own doc comment on why a network NUT server's own
// thresholds are outside Hoserva's reach.
func TestRenderUPSMonConf_NetworkOmitsLocalOnlyOverrides(t *testing.T) {
	state := UPSState{
		Connection:        UPSConnectionNetwork,
		NetworkHost:       "nut.example.lan",
		NetworkUPSName:    "office-ups",
		NetworkUsername:   "hoserva",
		NetworkPassword:   "secret",
		LowBatteryPercent: 20,
		RuntimeSeconds:    300,
	}
	got := RenderUPSMonConf(state)
	if got == "" {
		t.Fatal("RenderUPSMonConf returned an empty string")
	}
	for _, unexpected := range []string{"override.battery", "20", "300"} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("RenderUPSMonConf leaked a USB-only value %q into network mode:\n%s", unexpected, got)
		}
	}
}

func writeUPS(t *testing.T, g *Generator, state UPSState, revision int, now time.Time) {
	t.Helper()
	if err := g.WriteUPS(context.Background(), state, "settings ups", revision, now); err != nil {
		t.Fatalf("WriteUPS: %v", err)
	}
}

// TestWriteUPS_USBWritesAllFourFiles proves WriteUPS writes nut.conf,
// upsmon.conf, ups.conf and upsd.users for a USB-attached UPS, each with
// the doc 01 §2 header plus its own Render output.
func TestWriteUPS_USBWritesAllFourFiles(t *testing.T) {
	state := loadUPSState(t, "usb")
	g := NewGenerator(t.TempDir())
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	writeUPS(t, g, state, 1, now)

	cases := []struct {
		path string
		want string
	}{
		{PathNUTConf, RenderNUTConf(state)},
		{PathUPSMonConf, RenderUPSMonConf(state)},
		{PathUPSConf, RenderUPSConf(state)},
		{PathUPSDUsers, RenderUPSDUsers(state)},
	}
	for _, c := range cases {
		got, err := os.ReadFile(filepath.Join(g.Root, c.path))
		if err != nil {
			t.Fatalf("reading %s: %v", c.path, err)
		}
		want := Header("settings ups", 1, now) + c.want
		if string(got) != want {
			t.Fatalf("%s mismatch:\ngot:\n%s\nwant:\n%s", c.path, got, want)
		}
	}
}

// TestWriteUPS_NetworkWritesOnlyTwoFiles proves WriteUPS never writes
// ups.conf/upsd.users for a network NUT server — those files belong to
// the remote server's own configuration, not Hoserva's (RenderUPSConf's
// own doc comment).
func TestWriteUPS_NetworkWritesOnlyTwoFiles(t *testing.T) {
	state := loadUPSState(t, "network")
	g := NewGenerator(t.TempDir())
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	writeUPS(t, g, state, 1, now)

	for _, path := range []string{PathNUTConf, PathUPSMonConf} {
		if _, err := os.Stat(filepath.Join(g.Root, path)); err != nil {
			t.Fatalf("expected %s to exist: %v", path, err)
		}
	}
	for _, path := range []string{PathUPSConf, PathUPSDUsers} {
		if _, err := os.Stat(filepath.Join(g.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to not exist in network mode, got err = %v", path, err)
		}
	}
}

// TestWriteUPS_SwitchingFromUSBToNetworkRemovesLocalFiles proves the Q77
// reconciliation: a UPS previously configured over USB, then switched to
// a network NUT server, loses its now-stale local ups.conf and
// upsd.users rather than leaving a driver stanza for a UPS Hoserva no
// longer runs a local driver for.
func TestWriteUPS_SwitchingFromUSBToNetworkRemovesLocalFiles(t *testing.T) {
	usb := loadUPSState(t, "usb")
	g := NewGenerator(t.TempDir())
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	writeUPS(t, g, usb, 1, now)
	if _, err := os.Stat(filepath.Join(g.Root, PathUPSConf)); err != nil {
		t.Fatalf("expected %s to exist after the USB write: %v", PathUPSConf, err)
	}

	network := loadUPSState(t, "network")
	writeUPS(t, g, network, 2, now)

	for _, path := range []string{PathUPSConf, PathUPSDUsers} {
		if _, err := os.Stat(filepath.Join(g.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed after switching to network mode, got err = %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(g.Root, PathUPSMonConf)); err != nil {
		t.Fatalf("expected %s to still exist: %v", PathUPSMonConf, err)
	}
}

// TestWriteUPS_RefusesControlCharacterInField proves WriteUPS refuses a
// CR/LF (or other control character) in any user-controlled field before
// writing anything — the field otherwise lands unescaped in a generated
// NUT config line, letting it inject a new directive.
func TestWriteUPS_RefusesControlCharacterInField(t *testing.T) {
	state := loadUPSState(t, "usb")
	state.MonitorPassword = "s3cr3t\nSHUTDOWNCMD \"/bin/rm -rf /\""
	g := NewGenerator(t.TempDir())

	err := g.WriteUPS(context.Background(), state, "settings ups", 1, time.Now())
	if !errors.Is(err, ErrInvalidUPSField) {
		t.Fatalf("WriteUPS with a newline in monitor_password = %v, want ErrInvalidUPSField", err)
	}
	if _, statErr := os.Stat(filepath.Join(g.Root, PathNUTConf)); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s to not exist after a refused write", PathNUTConf)
	}
}

// TestCanWriteUPS_RefusesWhitespaceInField proves the same validation
// runs in the CanWriteUPS preflight, not only inside WriteUPS itself —
// a space in a MONITOR-line field (upsmon.conf(5)) would misalign its
// positional tokens.
func TestCanWriteUPS_RefusesWhitespaceInField(t *testing.T) {
	state := loadUPSState(t, "network")
	state.NetworkUsername = "hoserva admin"

	err := NewGenerator(t.TempDir()).CanWriteUPS(context.Background(), state)
	if !errors.Is(err, ErrInvalidUPSField) {
		t.Fatalf("CanWriteUPS with a space in network_username = %v, want ErrInvalidUPSField", err)
	}
}

// TestCanWriteUPS_RefusesExistingHostFile proves the preflight refuses
// before any file is written when one of the four target paths already
// exists and was never imported (Q76) — CanWriteShareFiles's own
// precedent, applied to WriteUPS's own file set.
func TestCanWriteUPS_RefusesExistingHostFile(t *testing.T) {
	state := loadUPSState(t, "usb")
	root := t.TempDir()
	g := NewGenerator(root)

	full := filepath.Join(root, PathUPSMonConf)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("MONITOR hand-edited@localhost 1 admin pass master\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := g.CanWriteUPS(context.Background(), state)
	if !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("CanWriteUPS with an existing unimported upsmon.conf = %v, want ErrExistingHostFile", err)
	}
}
