package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderIfupdownDHCP(t *testing.T) {
	got, err := RenderIfupdown(NetworkChange{Interface: "enp1s0", Method: MethodDHCP, DNS: []string{"1.1.1.1"}})
	if err != nil {
		t.Fatalf("RenderIfupdown: %v", err)
	}
	want := "auto enp1s0\niface enp1s0 inet dhcp\n    dns-nameservers 1.1.1.1\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderIfupdownStatic(t *testing.T) {
	got, err := RenderIfupdown(NetworkChange{
		Interface: "eth0",
		Method:    MethodStatic,
		Address:   "192.168.1.10",
		Prefix:    24,
		Gateway:   "192.168.1.1",
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
	})
	if err != nil {
		t.Fatalf("RenderIfupdown: %v", err)
	}
	if !strings.Contains(got, "address 192.168.1.10/24") {
		t.Fatalf("missing address: %s", got)
	}
	if !strings.Contains(got, "gateway 192.168.1.1") {
		t.Fatalf("missing gateway: %s", got)
	}
}

func TestRenderIfupdownRejectsShellMetacharacters(t *testing.T) {
	_, err := RenderIfupdown(NetworkChange{Interface: "eth0;reboot", Method: MethodDHCP})
	if !errors.Is(err, ErrNetworkInvalid) {
		t.Fatalf("err = %v, want ErrNetworkInvalid", err)
	}
}

func TestDetectPrefersNetworkManager(t *testing.T) {
	d := ExecDetector{
		LookPath: func(string) (string, error) { return "/bin/systemctl", nil },
		Run: func(_ context.Context, name string, args ...string) error {
			if name == "systemctl" && len(args) == 3 && args[2] == "NetworkManager" {
				return nil
			}
			return errors.New("inactive")
		},
	}
	backend, reason, err := d.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendNetworkManager {
		t.Fatalf("backend = %s", backend)
	}
	if reason == "" {
		t.Fatal("expected a read-only reason")
	}
}

func TestDetectIfupdownWhenIfupExists(t *testing.T) {
	d := ExecDetector{
		LookPath: func(file string) (string, error) {
			if file == "ifup" {
				return "/sbin/ifup", nil
			}
			return "", errors.New("not found")
		},
		Run: func(context.Context, string, ...string) error { return errors.New("inactive") },
	}
	backend, reason, err := d.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendIfupdown || reason != "" {
		t.Fatalf("backend=%s reason=%q", backend, reason)
	}
}

func newTestNetworkService(t *testing.T, window time.Duration) (*NetworkService, *MemoryRunner) {
	t.Helper()
	root := t.TempDir()
	runner := &MemoryRunner{}
	svc := &NetworkService{
		Generator: NewGenerator(root),
		Detector:  MemoryDetector{Backend: BackendIfupdown},
		Runner:    runner,
		Links: MemoryLinks{Ifaces: []Iface{{
			Name: "eth0", MAC: "02:00:00:00:00:01", Method: MethodDHCP, State: IfaceUp,
			Address: "10.0.2.15", Prefix: 24, Gateway: "10.0.2.2",
		}}},
		StateDir: t.TempDir(),
		Window:   window,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(svc.Close)
	return svc, runner
}

func TestApplyStartsConfirmWindowAndWritesManagedFile(t *testing.T) {
	svc, runner := newTestNetworkService(t, time.Hour)
	ctx := context.Background()
	st, err := svc.Apply(ctx, NetworkChange{
		Interface: "eth0",
		Method:    MethodStatic,
		Address:   "192.0.2.1",
		Prefix:    24,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st.Pending == nil || st.Pending.Interface != "eth0" {
		t.Fatalf("pending = %+v", st.Pending)
	}
	if len(runner.Calls) != 1 || runner.Calls[0] != "eth0" {
		t.Fatalf("runner calls = %v", runner.Calls)
	}
	raw, err := os.ReadFile(filepath.Join(svc.Generator.Root, PathIfupdown))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "address 192.0.2.1/24") {
		t.Fatalf("managed file: %s", raw)
	}
}

func TestConfirmKeepsNewFile(t *testing.T) {
	svc, _ := newTestNetworkService(t, time.Hour)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, NetworkChange{Interface: "eth0", Method: MethodDHCP}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Confirm(ctx)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if st.Pending != nil {
		t.Fatalf("pending after confirm: %+v", st.Pending)
	}
	if _, err := os.Stat(filepath.Join(svc.Generator.Root, PathIfupdown)); err != nil {
		t.Fatalf("managed file should remain: %v", err)
	}
}

func TestTimeoutRestoresPreviousFile(t *testing.T) {
	svc, runner := newTestNetworkService(t, 40*time.Millisecond)
	ctx := context.Background()
	first, err := RenderIfupdown(NetworkChange{Interface: "eth0", Method: MethodDHCP})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Generator.Write(ctx, File{Path: PathIfupdown, Command: "network", Body: []byte(first)}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, NetworkChange{
		Interface: "eth0", Method: MethodStatic, Address: "192.0.2.1", Prefix: 24,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := svc.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == nil {
			raw, err := os.ReadFile(filepath.Join(svc.Generator.Root, PathIfupdown))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "inet dhcp") {
				t.Fatalf("restored file missing dhcp: %s", raw)
			}
			if len(runner.Calls) < 2 {
				t.Fatalf("expected apply then revert, calls=%v", runner.Calls)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending change did not revert within the window")
}

func TestRecoverRestoresAfterProcessDeath(t *testing.T) {
	svc, _ := newTestNetworkService(t, time.Hour)
	ctx := context.Background()
	first, err := RenderIfupdown(NetworkChange{Interface: "eth0", Method: MethodDHCP})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Generator.Write(ctx, File{Path: PathIfupdown, Command: "network", Body: []byte(first)}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, NetworkChange{
		Interface: "eth0", Method: MethodStatic, Address: "192.0.2.1", Prefix: 24,
	}); err != nil {
		t.Fatal(err)
	}
	svc.Close()

	restored := &NetworkService{
		Generator: svc.Generator,
		Detector:  svc.Detector,
		Runner:    &MemoryRunner{},
		Links:     svc.Links,
		StateDir:  svc.StateDir,
		Window:    time.Hour,
		Now:       svc.Now,
	}
	t.Cleanup(restored.Close)
	if err := restored.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(svc.Generator.Root, PathIfupdown))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "inet dhcp") {
		t.Fatalf("recover did not restore dhcp: %s", raw)
	}
	st, err := restored.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != nil {
		t.Fatalf("pending after recover: %+v", st.Pending)
	}
}

func TestApplyRefusesNonIfupdown(t *testing.T) {
	svc, _ := newTestNetworkService(t, time.Hour)
	svc.Detector = MemoryDetector{Backend: BackendNetworkManager, Reason: readOnlyNetworkManager}
	_, err := svc.Apply(context.Background(), NetworkChange{Interface: "eth0", Method: MethodDHCP})
	if !errors.Is(err, ErrNetworkReadOnly) {
		t.Fatalf("err = %v, want ErrNetworkReadOnly", err)
	}
}

func TestConfirmExpiredAfterTimeout(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc, _ := newTestNetworkService(t, time.Millisecond)
	svc.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := svc.Apply(ctx, NetworkChange{Interface: "eth0", Method: MethodDHCP}); err != nil {
		t.Fatal(err)
	}
	svc.Now = func() time.Time { return now.Add(time.Second) }
	_, err := svc.Confirm(ctx)
	if !errors.Is(err, ErrNetworkConfirmExpired) {
		t.Fatalf("err = %v, want ErrNetworkConfirmExpired", err)
	}
}

func TestParseHexIPv4Gateway(t *testing.T) {
	got, err := parseHexIPv4("0102A8C0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.2.1" {
		t.Fatalf("got %s", got)
	}
}

func TestExecIfupdownRejectsInvalidName(t *testing.T) {
	err := ExecIfupdown{}.Apply(context.Background(), "eth0;id")
	if !errors.Is(err, ErrNetworkInvalid) {
		t.Fatalf("err = %v", err)
	}
}
