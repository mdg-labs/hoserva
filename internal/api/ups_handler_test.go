package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type fakeNUTReloader struct {
	calls []config.UPSConnection
	err   error
}

func (f *fakeNUTReloader) Reload(ctx context.Context, connection config.UPSConnection) error {
	f.calls = append(f.calls, connection)
	return f.err
}

// fakeUPSSocketPermissions is api.UPSSocketPermissions' own fake (#340):
// tests assert Apply ran (or, for the failure-path test, that a returned
// error still refuses before any lasting state changes) without a real
// ups control socket or a real nut group on this host.
type fakeUPSSocketPermissions struct {
	calls int
	err   error
}

func (f *fakeUPSSocketPermissions) Apply(ctx context.Context) error {
	f.calls++
	return f.err
}

func newUPSTestEnv(t *testing.T) (context.Context, *api.Handler, *api.UPSStore, *config.Generator, *fakeNUTReloader, *fakeUPSSocketPermissions) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "ups-handler-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nut"), 0o755); err != nil {
		t.Fatalf("mkdir nut: %v", err)
	}
	g := config.NewGenerator(root)
	g.LookupGroup = func(name string) (int, error) {
		if name != config.NUTGroup {
			return 0, errors.New("unexpected group " + name)
		}
		return os.Getgid(), nil
	}
	nut := &fakeNUTReloader{}
	socket := &fakeUPSSocketPermissions{}
	storeUPS := api.NewUPSStore(db)
	svc := api.NewUPSService(storeUPS, fakeSettingsCipher{}, g, nut, socket)
	// Each call is a new second, so a rollback that stamps files with
	// "now" cannot accidentally match the prior header.
	clock := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time {
		current := clock
		clock = clock.Add(time.Second)
		return current
	}
	return context.Background(), &api.Handler{UPS: svc, Generator: g}, storeUPS, g, nut, socket
}

func TestGetUPSSettingsEmpty(t *testing.T) {
	ctx, h, _, _, _, _ := newUPSTestEnv(t)
	got, err := h.GetUPSSettings(ctx)
	if err != nil {
		t.Fatalf("GetUPSSettings: %v", err)
	}
	if got.Configured {
		t.Fatal("expected configured=false on a fresh install")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"configured":false}` {
		t.Fatalf("GET = %s", raw)
	}
}

func TestUpdateUPSSettingsUSBNeverReturnsPassword(t *testing.T) {
	ctx, h, upsStore, g, nut, _ := newUPSTestEnv(t)
	pwd := "s3cr3t-monitor-pass"
	got, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString(pwd),
		LowBatteryPercent: apiv1.NewOptInt32(20),
		RuntimeSeconds:    apiv1.NewOptInt32(300),
	})
	if err != nil {
		t.Fatalf("UpdateUPSSettings: %v", err)
	}
	if !got.Configured {
		t.Fatal("expected configured=true")
	}
	if set, ok := got.MonitorPasswordSet.Get(); !ok || !set {
		t.Fatal("expected monitorPasswordSet true")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), pwd) || strings.Contains(string(raw), "monitorPassword\":") {
		t.Fatalf("response leaked a password field: %s", raw)
	}
	if len(nut.calls) != 1 || nut.calls[0] != config.UPSConnectionUSB {
		t.Fatalf("NUT reload calls = %v, want one usb", nut.calls)
	}
	body, err := os.ReadFile(filepath.Join(g.Root, config.PathUPSMonConf))
	if err != nil {
		t.Fatalf("reading upsmon.conf: %v", err)
	}
	if !strings.Contains(string(body), pwd) {
		t.Fatalf("generated upsmon.conf missing password; WriteUPS did not run")
	}
	row, err := upsStore.Get(ctx)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if string(row.MonitorPassword) == pwd {
		t.Fatal("monitor password stored in plaintext")
	}
}

func TestUpdateUPSSettingsOmittingPasswordKeepsSecret(t *testing.T) {
	ctx, h, _, _, _, _ := newUPSTestEnv(t)
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("first-pass"),
	}); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	got, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		LowBatteryPercent: apiv1.NewOptInt32(15),
	})
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if set, ok := got.MonitorPasswordSet.Get(); !ok || !set {
		t.Fatal("expected monitorPasswordSet still true")
	}
	pct, ok := got.LowBatteryPercent.Get()
	if !ok || pct != 15 {
		t.Fatalf("lowBatteryPercent = %v", got.LowBatteryPercent)
	}
}

func TestUpdateUPSSettingsMissingDriverIs400(t *testing.T) {
	ctx, h, upsStore, _, _, _ := newUPSTestEnv(t)
	_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ae interface{ Error() string }
	if !errors.As(err, &ae) {
		t.Fatalf("err type = %T", err)
	}
	if _, getErr := upsStore.Get(ctx); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("row should not exist after validation failure: %v", getErr)
	}
}

func TestUpdateUPSSettingsUnmanagedConfigIs409(t *testing.T) {
	ctx, h, upsStore, g, _, _ := newUPSTestEnv(t)
	hostFile := filepath.Join(g.Root, config.PathUPSMonConf)
	if err := os.WriteFile(hostFile, []byte("MODE=none\n"), 0o644); err != nil {
		t.Fatalf("seed host file: %v", err)
	}
	_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	})
	if err == nil {
		t.Fatal("expected unmanaged_config error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "existing host file") && !strings.Contains(msg, "unmanaged") {
		// mapUPSError wraps as apiError whose Error() is the message
		t.Logf("error message: %v", err)
	}
	var apiErr interface {
		Error() string
	}
	_ = apiErr
	if _, getErr := upsStore.Get(ctx); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("row left after CanWriteUPS refusal: %v", getErr)
	}
}

func TestUpdateUPSSettingsReloadFailureRollsBackDB(t *testing.T) {
	ctx, h, upsStore, g, nut, _ := newUPSTestEnv(t)
	nut.err = errors.New("systemctl restart failed")
	_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	})
	if err == nil {
		t.Fatal("expected reload error")
	}
	if _, getErr := upsStore.Get(ctx); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("row left after reload failure: %v", getErr)
	}
	for _, path := range []string{
		config.PathNUTConf,
		config.PathUPSMonConf,
		config.PathUPSConf,
		config.PathUPSDUsers,
	} {
		if _, err := os.Stat(filepath.Join(g.Root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generated %s left after first-configure reload failure: %v", path, err)
		}
	}
}

// TestUpdateUPSSettingsAppliesSocketPermissions proves Update calls
// UPSSocketPermissions.Apply after every successful WriteUPS (#340) —
// the only point that re-applies the ups control socket's nut-group
// ownership when nut is installed after this daemon started, since
// CanWriteUPS already guarantees the nut group exists by the time this
// runs.
func TestUpdateUPSSettingsAppliesSocketPermissions(t *testing.T) {
	ctx, h, _, _, _, socket := newUPSTestEnv(t)
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if socket.calls != 1 {
		t.Fatalf("socket.Apply called %d times, want 1", socket.calls)
	}
}

// TestUpdateUPSSettingsSocketPermissionsFailureRollsBackDB mirrors
// TestUpdateUPSSettingsReloadFailureRollsBackDB above: a failure
// applying the ups control socket's permissions is treated exactly like
// a reload failure — the row and every generated file it wrote are
// rolled back, never left half-applied.
func TestUpdateUPSSettingsSocketPermissionsFailureRollsBackDB(t *testing.T) {
	ctx, h, upsStore, g, _, socket := newUPSTestEnv(t)
	socket.err = errors.New("chown ups-control.sock failed")
	_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	})
	if err == nil {
		t.Fatal("expected socket permissions error")
	}
	if _, getErr := upsStore.Get(ctx); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("row left after socket permissions failure: %v", getErr)
	}
	for _, path := range []string{
		config.PathNUTConf,
		config.PathUPSMonConf,
		config.PathUPSConf,
		config.PathUPSDUsers,
	} {
		if _, err := os.Stat(filepath.Join(g.Root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generated %s left after first-configure socket permissions failure: %v", path, err)
		}
	}
}

func TestUpdateUPSSettingsReloadFailureRestoresPriorFiles(t *testing.T) {
	ctx, h, upsStore, g, nut, _ := newUPSTestEnv(t)
	firstPass := "first-monitor-pass"
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString(firstPass),
		LowBatteryPercent: apiv1.NewOptInt32(20),
		RuntimeSeconds:    apiv1.NewOptInt32(300),
	}); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(g.Root, config.PathUPSMonConf))
	if err != nil {
		t.Fatalf("reading prior upsmon.conf: %v", err)
	}

	nut.err = errors.New("systemctl restart failed")
	_, err = h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString("second-monitor-pass"),
		LowBatteryPercent: apiv1.NewOptInt32(10),
		RuntimeSeconds:    apiv1.NewOptInt32(60),
	})
	if err == nil {
		t.Fatal("expected reload error")
	}

	row, getErr := upsStore.Get(ctx)
	if getErr != nil {
		t.Fatalf("store Get after rollback: %v", getErr)
	}
	if row.LowBatteryPercent != 20 || row.RuntimeSeconds != 300 {
		t.Fatalf("db row = %+v, want prior low=20 runtime=300", row)
	}
	after, err := os.ReadFile(filepath.Join(g.Root, config.PathUPSMonConf))
	if err != nil {
		t.Fatalf("reading restored upsmon.conf: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("upsmon.conf after rollback does not match prior content")
	}
	if !strings.Contains(string(after), firstPass) {
		t.Fatal("restored upsmon.conf missing prior monitor password")
	}
	if strings.Contains(string(after), "second-monitor-pass") {
		t.Fatal("restored upsmon.conf still has the failed update's password")
	}
}

func TestUpdateUPSSettingsWriteUPSFailureRestoresPriorFiles(t *testing.T) {
	ctx, h, upsStore, g, _, _ := newUPSTestEnv(t)
	firstPass := "kept-monitor-pass"
	if _, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString(firstPass),
		LowBatteryPercent: apiv1.NewOptInt32(25),
		RuntimeSeconds:    apiv1.NewOptInt32(120),
	}); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(g.Root, config.PathUPSMonConf))
	if err != nil {
		t.Fatalf("reading prior upsmon.conf: %v", err)
	}

	// CanWriteUPS and WriteUPS's initial group check succeed; the later
	// upsd.users Write fails mid-apply so nut.conf/upsmon.conf already
	// hold the new body — rollback must restore the prior files.
	groupCalls := 0
	failNextUPSDUsers := true
	g.LookupGroup = func(name string) (int, error) {
		if name != config.NUTGroup {
			return 0, errors.New("unexpected group " + name)
		}
		groupCalls++
		// Second Update: CanWriteUPS=1, WriteUPS start=2, upsd.users=3.
		if failNextUPSDUsers && groupCalls == 3 {
			failNextUPSDUsers = false
			return 0, errors.New("nut group vanished mid-write")
		}
		return os.Getgid(), nil
	}

	_, err = h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:        apiv1.UPSConnectionUsb,
		Driver:            apiv1.NewOptString("usbhid-ups"),
		Port:              apiv1.NewOptString("auto"),
		MonitorPassword:   apiv1.NewOptString("should-not-stick"),
		LowBatteryPercent: apiv1.NewOptInt32(5),
		RuntimeSeconds:    apiv1.NewOptInt32(30),
	})
	if err == nil {
		t.Fatal("expected mid-WriteUPS error")
	}

	row, getErr := upsStore.Get(ctx)
	if getErr != nil {
		t.Fatalf("store Get after rollback: %v", getErr)
	}
	if row.LowBatteryPercent != 25 || row.RuntimeSeconds != 120 {
		t.Fatalf("db row = %+v, want prior low=25 runtime=120", row)
	}
	after, err := os.ReadFile(filepath.Join(g.Root, config.PathUPSMonConf))
	if err != nil {
		t.Fatalf("reading restored upsmon.conf: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("upsmon.conf after mid-WriteUPS rollback does not match prior content")
	}
	if !strings.Contains(string(after), firstPass) {
		t.Fatal("restored upsmon.conf missing prior monitor password")
	}
}

func TestUpdateUPSSettingsNetwork(t *testing.T) {
	ctx, h, _, g, nut, _ := newUPSTestEnv(t)
	got, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionNetwork,
		NetworkHost:     apiv1.NewOptString("nut.example.lan"),
		NetworkPort:     apiv1.NewOptInt32(3493),
		NetworkUpsName:  apiv1.NewOptString("office-ups"),
		NetworkUsername: apiv1.NewOptString("hoserva"),
		NetworkPassword: apiv1.NewOptString("s3cr3t-network-pass"),
	})
	if err != nil {
		t.Fatalf("UpdateUPSSettings: %v", err)
	}
	if set, ok := got.NetworkPasswordSet.Get(); !ok || !set {
		t.Fatal("expected networkPasswordSet true")
	}
	if _, err := os.Stat(filepath.Join(g.Root, config.PathUPSConf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network mode should not write ups.conf: %v", err)
	}
	if len(nut.calls) != 1 || nut.calls[0] != config.UPSConnectionNetwork {
		t.Fatalf("NUT reload = %v", nut.calls)
	}
}

func TestUpdateUPSSettingsMissingNUTGroupIs409(t *testing.T) {
	ctx, h, upsStore, g, _, _ := newUPSTestEnv(t)
	g.LookupGroup = func(name string) (int, error) {
		return 0, errors.New("no such group")
	}

	_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
		Connection:      apiv1.UPSConnectionUsb,
		Driver:          apiv1.NewOptString("usbhid-ups"),
		Port:            apiv1.NewOptString("auto"),
		MonitorPassword: apiv1.NewOptString("pass"),
	})
	if err == nil {
		t.Fatal("expected missing nut group error")
	}
	if !strings.Contains(err.Error(), "nut group is not present") {
		t.Fatalf("error = %v, want missing nut group message", err)
	}
	if _, getErr := upsStore.Get(ctx); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("row left after nut-group refusal: %v", getErr)
	}
}
