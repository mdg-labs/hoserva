package main

import (
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPSPersist_AtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := newHTTPSControl(dir, "", "", nil, 8008, false, tls.Certificate{})
	if err := h.SetAllowAllSources(true); err != nil {
		t.Fatal(err)
	}
	if err := h.SetListenPort(8443); err != nil {
		t.Fatal(err)
	}
	got, ok := loadPersistedHTTPS(dir)
	if !ok {
		t.Fatal("expected persisted HTTPS settings")
	}
	if !got.AllowAllSources || got.ListenPort != 8443 {
		t.Fatalf("persisted = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "https-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("https-settings.json mode = %o, want 0600", perm)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "https-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("persisted file is not valid JSON: %s", raw)
	}
}

func TestHTTPSPersist_ReportsError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	h := newHTTPSControl(dir, "", "", nil, 8008, false, tls.Certificate{})
	if err := h.SetAllowAllSources(true); err == nil {
		t.Fatal("persist into a missing directory must fail")
	}
}
