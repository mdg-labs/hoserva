package main

import (
	"context"
	"strings"
	"testing"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestNUTReloaderUSBRestartsDriverServerMonitor(t *testing.T) {
	r := disk.NewFakeRunner()
	units := []string{
		"nut-driver-enumerator.service",
		"nut-driver@hoserva-ups.service",
		"nut-server.service",
		"nut-monitor.service",
	}
	for _, unit := range units {
		r.Script("systemctl", []string{"show", "--property=LoadState", "--value", unit}, []byte("loaded\n"), nil)
		r.Script("systemctl", []string{"restart", unit}, nil, nil)
	}

	if err := newNUTReloader(r).Reload(context.Background(), cfggen.UPSConnectionUSB); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 8 {
		t.Fatalf("calls = %d, want 8 (show+restart × 4)", len(calls))
	}
}

func TestNUTReloaderNetworkOnlyRestartsMonitor(t *testing.T) {
	r := disk.NewFakeRunner()
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "nut-monitor.service"}, []byte("loaded\n"), nil)
	r.Script("systemctl", []string{"restart", "nut-monitor.service"}, nil, nil)

	if err := newNUTReloader(r).Reload(context.Background(), cfggen.UPSConnectionNetwork); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d (%v), want 2", len(calls), calls)
	}
	for _, c := range calls {
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "nut-monitor.service") {
			t.Fatalf("network reload must only touch nut-monitor, got %v", c)
		}
	}
}

func TestNUTReloaderSkipsNotFoundUnits(t *testing.T) {
	r := disk.NewFakeRunner()
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "nut-monitor.service"}, []byte("not-found\n"), nil)

	if err := newNUTReloader(r).Reload(context.Background(), cfggen.UPSConnectionNetwork); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(r.Calls()) != 1 {
		t.Fatalf("calls = %d, want only the LoadState query", len(r.Calls()))
	}
}
