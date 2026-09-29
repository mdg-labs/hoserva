package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
)

// After a config import replaced the database, Regenerate writes the NUT
// files from the restored settings row, byte for byte what the settings
// save wrote, and reloads the units.
func TestUPSRegenerateRewritesTheNUTFilesFromTheStoredSettings(t *testing.T) {
	ctx, h, _, g, nut, socket := newUPSTestEnv(t)
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString("stored-pass"),
		LowBatteryPercent: apiv1.NewOptInt32(20),
	}); err != nil {
		t.Fatalf("UpdateUPSSettings: %v", err)
	}
	paths := []string{config.PathNUTConf, config.PathUPSConf, config.PathUPSDUsers, config.PathUPSMonConf}
	want := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(g.Root, p))
		if err != nil {
			t.Fatal(err)
		}
		want[p] = string(b)
		if err := os.Remove(filepath.Join(g.Root, p)); err != nil {
			t.Fatal(err)
		}
	}
	nut.calls = nil
	sockets := socket.calls

	if err := h.UPS.Regenerate(ctx); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(g.Root, p))
		if err != nil || string(b) != want[p] {
			t.Errorf("%s = %q (%v), want what the save wrote", p, b, err)
		}
	}
	if len(nut.calls) != 1 || nut.calls[0] != config.UPSConnectionUSB {
		t.Errorf("NUT reloads = %v, want one usb", nut.calls)
	}
	if socket.calls != sockets+1 {
		t.Errorf("the ups control socket permissions were applied %d times, want once", socket.calls-sockets)
	}
}

// With no UPS settings in the restored database the files Hoserva wrote for
// the settings the import replaced are removed, and nothing is reloaded.
func TestUPSRegenerateWithNoStoredSettingsRemovesTheManagedFiles(t *testing.T) {
	ctx, h, upsStore, g, nut, _ := newUPSTestEnv(t)
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("stored-pass"),
	}); err != nil {
		t.Fatalf("UpdateUPSSettings: %v", err)
	}
	if err := upsStore.Delete(ctx); err != nil {
		t.Fatalf("deleting the settings row: %v", err)
	}
	nut.calls = nil

	if err := h.UPS.Regenerate(ctx); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	for _, p := range []string{config.PathNUTConf, config.PathUPSConf, config.PathUPSDUsers, config.PathUPSMonConf} {
		if _, err := os.Stat(filepath.Join(g.Root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", p, err)
		}
	}
	if len(nut.calls) != 0 {
		t.Errorf("NUT reloads = %v, want none", nut.calls)
	}
}

func TestUPSRegenerateReportsAFailedReload(t *testing.T) {
	ctx, h, _, _, nut, _ := newUPSTestEnv(t)
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("stored-pass"),
	}); err != nil {
		t.Fatalf("UpdateUPSSettings: %v", err)
	}
	nut.err = os.ErrPermission
	err := h.UPS.Regenerate(ctx)
	if err == nil || !strings.Contains(err.Error(), "reloading nut") {
		t.Fatalf("Regenerate = %v, want the reload failure", err)
	}
}
