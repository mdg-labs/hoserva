package api_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newDestinationHandler builds a Handler whose backup.Service persists
// destinations in the real, migrated schema through
// api.BackupDestinationStore — the wiring cmd/hoservad uses.
func newDestinationHandler(t *testing.T, hasPassphrase bool) (*api.Handler, *backup.Service, *backup.FakeRclone) {
	t.Helper()
	db := openTestDB(t)
	rclone := &backup.FakeRclone{}
	svc := &backup.Service{
		DB:                db,
		Store:             api.NewBackupDestinationStore(db),
		Rclone:            rclone,
		Secrets:           &backup.FakeSecretSource{Passphrase: "backup-pass", HasPass: hasPassphrase},
		Cipher:            backup.FakeSecretCipher{},
		DestinationCipher: backup.FakeSecretCipher{},
	}
	return &api.Handler{Backup: svc}, svc, rclone
}

func s3Body() *apiv1.CreateBackupDestinationRequest {
	return &apiv1.CreateBackupDestinationRequest{
		Name: "Backblaze", Type: apiv1.BackupDestinationTypeS3, Path: "bucket/hoserva",
		Options: apiv1.OptCreateBackupDestinationRequestOptions{Set: true, Value: apiv1.CreateBackupDestinationRequestOptions{"access_key_id": "AKIA"}},
		Secrets: apiv1.OptCreateBackupDestinationRequestSecrets{Set: true, Value: apiv1.CreateBackupDestinationRequestSecrets{"secret_access_key": "s3cr3t-key"}},
	}
}

func TestBackupDestinations_NotConfiguredIs501(t *testing.T) {
	ctx := context.Background()
	h := &api.Handler{}
	_, err := h.ListBackupDestinations(ctx)
	if ae := apiError(t, h, err); ae.StatusCode != 501 || ae.Response.Code != "not_configured" {
		t.Fatalf("status = %d %q, want 501 not_configured", ae.StatusCode, ae.Response.Code)
	}
}

func TestBackupDestinations_CreateListDeleteRoundTrip(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newDestinationHandler(t, true)

	created, err := h.CreateBackupDestination(ctx, s3Body())
	if err != nil {
		t.Fatalf("CreateBackupDestination: %v", err)
	}
	if !created.Encrypt || !created.HasSecrets || created.Stale || created.ID == "" {
		t.Fatalf("created = %+v, want an encrypting destination with credentials and no staleness yet", created)
	}

	list, err := h.ListBackupDestinations(ctx)
	if err != nil || len(list.Destinations) != 1 {
		t.Fatalf("ListBackupDestinations = %+v, %v", list, err)
	}
	got := list.Destinations[0]
	if got.ID != created.ID || got.Options.Value["access_key_id"] != "AKIA" || got.Retention.Daily != backup.DefaultRetentionDaily {
		t.Fatalf("listed = %+v", got)
	}
	if strings.Contains(got.Path+got.Name, "s3cr3t") {
		t.Fatal("a credential is in the list response")
	}

	if err := h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: created.ID}); err != nil {
		t.Fatalf("DeleteBackupDestination: %v", err)
	}
	list, _ = h.ListBackupDestinations(ctx)
	if len(list.Destinations) != 0 {
		t.Fatalf("destination still listed after delete: %+v", list.Destinations)
	}
	err = h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: created.ID})
	if ae := apiError(t, h, err); ae.StatusCode != 404 || ae.Response.Code != "backup_destination_not_found" {
		t.Fatalf("second delete = %d %q, want 404 backup_destination_not_found", ae.StatusCode, ae.Response.Code)
	}
}

func TestBackupDestinations_RemoteWithoutPassphraseIs400(t *testing.T) {
	ctx := context.Background()
	h, _, rclone := newDestinationHandler(t, false)

	_, err := h.CreateBackupDestination(ctx, s3Body())
	if ae := apiError(t, h, err); ae.StatusCode != 400 || ae.Response.Code != "backup_passphrase_required" {
		t.Fatalf("status = %d %q, want 400 backup_passphrase_required", ae.StatusCode, ae.Response.Code)
	}
	if len(rclone.Calls()) != 0 {
		t.Fatal("rclone ran for a refused request")
	}
	list, _ := h.ListBackupDestinations(ctx)
	if len(list.Destinations) != 0 {
		t.Fatalf("a refused destination was stored: %+v", list.Destinations)
	}
}

func TestBackupDestinations_MissingRcloneReturnsInstallCommand(t *testing.T) {
	ctx := context.Background()
	h, _, rclone := newDestinationHandler(t, true)
	rclone.Missing = true

	_, err := h.CreateBackupDestination(ctx, s3Body())
	ae := apiError(t, h, err)
	if ae.StatusCode != 424 || ae.Response.Code != "rclone_missing" {
		t.Fatalf("status = %d %q, want 424 rclone_missing", ae.StatusCode, ae.Response.Code)
	}
	if !strings.Contains(ae.Response.Message, "sudo apt install rclone") {
		t.Fatalf("message = %q, want the install command", ae.Response.Message)
	}

	local := &apiv1.CreateBackupDestinationRequest{Name: "Disk", Type: apiv1.BackupDestinationTypeLocal, Path: "/mnt/disks/backup"}
	if _, err := h.CreateBackupDestination(ctx, local); err != nil {
		t.Fatalf("a local destination must not need rclone: %v", err)
	}
}

func TestBackupDestinations_InvalidAndDuplicate(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newDestinationHandler(t, true)

	_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "Disk", Type: apiv1.BackupDestinationTypeLocal, Path: "relative"})
	if ae := apiError(t, h, err); ae.StatusCode != 400 || ae.Response.Code != "backup_destination_invalid" {
		t.Fatalf("status = %d %q, want 400 backup_destination_invalid", ae.StatusCode, ae.Response.Code)
	}

	local := &apiv1.CreateBackupDestinationRequest{Name: "Disk", Type: apiv1.BackupDestinationTypeLocal, Path: "/mnt/disks/backup"}
	if _, err := h.CreateBackupDestination(ctx, local); err != nil {
		t.Fatal(err)
	}
	_, err = h.CreateBackupDestination(ctx, local)
	if ae := apiError(t, h, err); ae.StatusCode != 409 || ae.Response.Code != "backup_destination_exists" {
		t.Fatalf("status = %d %q, want 409 backup_destination_exists", ae.StatusCode, ae.Response.Code)
	}
}

func TestBackupDestinations_TestOperationWritesReadsBackAndDeletes(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newDestinationHandler(t, true)
	dir := filepath.Join(t.TempDir(), "probe")
	created, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "Disk", Type: apiv1.BackupDestinationTypeLocal, Path: dir})
	if err != nil {
		t.Fatal(err)
	}

	res, err := h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: created.ID})
	if err != nil || !res.Success {
		t.Fatalf("TestBackupDestination = %+v, %v", res, err)
	}

	remote, err := h.CreateBackupDestination(ctx, s3Body())
	if err != nil {
		t.Fatal(err)
	}
	res, err = h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: remote.ID})
	if err != nil || !res.Success {
		t.Fatalf("remote TestBackupDestination = %+v, %v", res, err)
	}

	_, err = h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: "nope"})
	if ae := apiError(t, h, err); ae.StatusCode != 404 {
		t.Fatalf("unknown id status = %d, want 404", ae.StatusCode)
	}
}

func TestBackupDestinations_TestReportsAFailureAsAResult(t *testing.T) {
	ctx := context.Background()
	h, _, rclone := newDestinationHandler(t, true)
	remote, err := h.CreateBackupDestination(ctx, s3Body())
	if err != nil {
		t.Fatal(err)
	}
	rclone.Fail = map[string]error{"copy": &backup.RcloneExitError{Code: 1, Output: "access denied"}}

	res, err := h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: remote.ID})
	if err != nil {
		t.Fatalf("TestBackupDestination: %v", err)
	}
	if res.Success || !strings.Contains(res.Error.Value, "access denied") {
		t.Fatalf("result = %+v, want a failure carrying the reason", res)
	}
}

func TestBackupDestinationStore_RoundTripsAndTracksSuccess(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := api.NewBackupDestinationStore(db)
	created := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)

	if _, err := st.GetDestination(ctx, "nope"); !errors.Is(err, backup.ErrDestinationNotFound) {
		t.Fatalf("GetDestination(unknown) = %v, want ErrDestinationNotFound", err)
	}
	d := backup.Destination{
		ID: "dest-1", Name: "Bucket", Type: backup.TypeS3, Path: "b/p", Enabled: true, Encrypt: true,
		Retention:     backup.Retention{Daily: 3, Weekly: 2, Monthly: 1},
		Options:       map[string]string{"access_key_id": "AKIA"},
		SealedSecrets: []byte{1, 2, 3},
		CreatedAt:     created,
	}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatalf("CreateDestination: %v", err)
	}
	dup := d
	dup.ID = "dest-2"
	dup.Name = "BUCKET"
	if err := st.CreateDestination(ctx, dup); err == nil {
		t.Fatal("two destinations with the same name (case-insensitive) were accepted")
	}

	got, err := st.GetDestination(ctx, "dest-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != backup.TypeS3 || got.Retention != d.Retention || got.Options["access_key_id"] != "AKIA" ||
		string(got.SealedSecrets) != string([]byte{1, 2, 3}) || !got.CreatedAt.Equal(created) || got.LastSuccessfulBackupAt != nil {
		t.Fatalf("got = %+v", got)
	}

	alertedAt := created.Add(72 * time.Hour)
	earlier := created.Add(time.Hour)
	if err := st.MarkStaleAlerted(ctx, "dest-1", &earlier, alertedAt); !errors.Is(err, backup.ErrDestinationNotFound) {
		t.Fatalf("a mark against a success time the row does not hold = %v, want ErrDestinationNotFound", err)
	}
	if got, _ = st.GetDestination(ctx, "dest-1"); got.StaleAlertedAt != nil {
		t.Fatal("a mark against a changed row was recorded")
	}
	if err := st.MarkStaleAlerted(ctx, "dest-1", nil, alertedAt); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetDestination(ctx, "dest-1"); got.StaleAlertedAt == nil {
		t.Fatal("the stale alert was not recorded")
	}
	success := created.Add(96 * time.Hour)
	if err := st.RecordBackupSuccess(ctx, "dest-1", success); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetDestination(ctx, "dest-1")
	if got.LastSuccessfulBackupAt == nil || !got.LastSuccessfulBackupAt.Equal(success) || got.StaleAlertedAt != nil {
		t.Fatalf("after a success: %+v, want the time recorded and the alert cleared", got)
	}
	if err := st.MarkStaleAlerted(ctx, "dest-1", got.LastSuccessfulBackupAt, alertedAt); err != nil {
		t.Fatalf("a mark against the success time the row holds: %v", err)
	}
	if got, _ = st.GetDestination(ctx, "dest-1"); got.StaleAlertedAt == nil {
		t.Fatal("the stale alert after a success was not recorded")
	}

	local := backup.Destination{ID: "boot", Name: "Boot", Type: backup.TypeLocal, Path: "/x", Enabled: true, CreatedAt: created}
	if err := st.CreateDestination(ctx, local); err != nil {
		t.Fatalf("a local destination (no options, no secrets): %v", err)
	}

	if err := st.DeleteDestination(ctx, "dest-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDestination(ctx, "dest-1"); !errors.Is(err, backup.ErrDestinationNotFound) {
		t.Fatalf("second delete = %v, want ErrDestinationNotFound", err)
	}
	if err := st.RecordBackupSuccess(ctx, "dest-1", success); !errors.Is(err, backup.ErrDestinationNotFound) {
		t.Fatalf("RecordBackupSuccess(deleted) = %v, want ErrDestinationNotFound", err)
	}
}

func TestBackupDestinationStore_SecretsForConfigBackup(t *testing.T) {
	ctx := context.Background()
	st := api.NewBackupDestinationStore(openTestDB(t))
	now := time.Now().UTC()
	for _, d := range []backup.Destination{
		{ID: "a", Name: "A", Type: backup.TypeLocal, Path: "/a", CreatedAt: now},
		{ID: "b", Name: "B", Type: backup.TypeS3, Path: "b/p", SealedSecrets: []byte{9}, Encrypt: true, CreatedAt: now},
	} {
		if err := st.CreateDestination(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.BackupDestinationSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RowID != "b" || got[0].Table != "backup_destinations" || got[0].Column != "secrets" {
		t.Fatalf("secrets = %+v, want only destination b's credentials", got)
	}
}

// The second default's insert fails: the first must not survive on its
// own, or every later start would see one row and never seed the rest.
func TestSeedDestinations_AFailedSeedLeavesNothingAndARetrySeedsEveryDefault(t *testing.T) {
	ctx := context.Background()
	_, svc, _ := newDestinationHandler(t, false)

	defaults := backup.DefaultDestinations()
	broken := append([]backup.Destination(nil), defaults...)
	broken[1].ID = broken[0].ID
	if err := svc.SeedDestinations(ctx, broken); err == nil {
		t.Fatal("a seed whose second insert fails reported success")
	}
	dests, err := svc.ListDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 0 {
		t.Fatalf("a failed seed left %d destination(s) behind: %+v", len(dests), dests)
	}

	if err := svc.SeedDestinations(ctx, defaults); err != nil {
		t.Fatalf("retry: %v", err)
	}
	dests, err = svc.ListDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 2 || dests[0].ID != backup.DefaultBootID || dests[1].ID != backup.DefaultPoolID {
		t.Fatalf("after the retry destinations = %+v, want the boot and pool defaults", dests)
	}

	if err := svc.SeedDestinations(ctx, defaults[:1]); err != nil {
		t.Fatalf("a seed over existing destinations must be a no-op: %v", err)
	}
	if dests, _ = svc.ListDestinations(ctx); len(dests) != 2 {
		t.Fatalf("a seed over existing destinations changed them: %+v", dests)
	}
}

func TestBackupDestinations_UpdateChangesEnabledAndRetentionInPlace(t *testing.T) {
	ctx := context.Background()
	h, svc, _ := newDestinationHandler(t, true)
	created, err := h.CreateBackupDestination(ctx, s3Body())
	if err != nil {
		t.Fatal(err)
	}
	success := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	if err := svc.Store.RecordBackupSuccess(ctx, created.ID, success); err != nil {
		t.Fatal(err)
	}

	params := apiv1.UpdateBackupDestinationParams{DestinationId: created.ID}
	got, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
		Enabled:   apiv1.NewOptBool(false),
		Retention: apiv1.NewOptBackupRetention(apiv1.BackupRetention{Daily: 3, Weekly: 0, Monthly: 2}),
	}, params)
	if err != nil {
		t.Fatalf("UpdateBackupDestination: %v", err)
	}
	if got.ID != created.ID || got.Enabled || got.Retention != (apiv1.BackupRetention{Daily: 3, Monthly: 2}) {
		t.Fatalf("updated = %+v, want the same destination disabled with retention 3/0/2", got)
	}
	if !got.HasSecrets || !got.LastSuccessfulBackupAt.Set || !got.LastSuccessfulBackupAt.Value.Equal(success) || !got.Encrypt || got.Path != created.Path {
		t.Fatalf("updated = %+v, want credentials, last success, path and encryption kept", got)
	}

	list, err := h.ListBackupDestinations(ctx)
	if err != nil || len(list.Destinations) != 1 || list.Destinations[0].Enabled || list.Destinations[0].Retention.Daily != 3 {
		t.Fatalf("list after update = %+v, %v", list, err)
	}

	back, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(true)}, params)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Enabled || back.Retention.Daily != 3 || back.Retention.Monthly != 2 {
		t.Fatalf("enabled-only update = %+v, want the retention left alone", back)
	}
	noop, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{}, params)
	if err != nil || !noop.Enabled || noop.Retention.Daily != 3 {
		t.Fatalf("an empty update = %+v, %v, want the destination unchanged", noop, err)
	}
}

func TestBackupDestinations_UpdateRefusals(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newDestinationHandler(t, true)
	created, err := h.CreateBackupDestination(ctx, s3Body())
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(true)}, apiv1.UpdateBackupDestinationParams{DestinationId: "nope"})
	if ae := apiError(t, h, err); ae.StatusCode != 404 || ae.Response.Code != "backup_destination_not_found" {
		t.Fatalf("unknown id = %d %q, want 404 backup_destination_not_found", ae.StatusCode, ae.Response.Code)
	}

	for name, r := range map[string]apiv1.BackupRetention{
		"none kept":       {},
		"over the bound":  {Daily: 1001},
		"negative weekly": {Daily: 1, Weekly: -1},
	} {
		_, err = h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
			Enabled:   apiv1.NewOptBool(false),
			Retention: apiv1.NewOptBackupRetention(r),
		}, apiv1.UpdateBackupDestinationParams{DestinationId: created.ID})
		if ae := apiError(t, h, err); ae.StatusCode != 400 || ae.Response.Code != "backup_destination_invalid" {
			t.Fatalf("%s = %d %q, want 400 backup_destination_invalid", name, ae.StatusCode, ae.Response.Code)
		}
	}
	list, _ := h.ListBackupDestinations(ctx)
	if !list.Destinations[0].Enabled || list.Destinations[0].Retention.Daily != backup.DefaultRetentionDaily {
		t.Fatalf("a refused update changed the destination: %+v", list.Destinations[0])
	}

	nh := &api.Handler{}
	_, err = nh.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{}, apiv1.UpdateBackupDestinationParams{DestinationId: created.ID})
	if ae := apiError(t, nh, err); ae.StatusCode != 501 {
		t.Fatalf("without a backup service = %d, want 501", ae.StatusCode)
	}
}

func TestBackupDestinations_UpdateReenableRestartsStaleness(t *testing.T) {
	ctx := context.Background()
	h, svc, _ := newDestinationHandler(t, true)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if err := svc.Store.CreateDestination(ctx, backup.Destination{ID: "paused", Name: "Paused", Type: backup.TypeLocal, Path: "/mnt/disks/paused", Enabled: false, CreatedAt: old.Add(-24 * time.Hour), LastSuccessfulBackupAt: &old, Retention: backup.Retention{Daily: 1}}); err != nil {
		t.Fatal(err)
	}
	params := apiv1.UpdateBackupDestinationParams{DestinationId: "paused"}

	got, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(true)}, params)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale {
		t.Fatal("a destination switched back on after a week was reported stale at once")
	}
	if _, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(false)}, params); err != nil {
		t.Fatal(err)
	}
	again, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(true)}, params)
	if err != nil || again.Stale {
		t.Fatalf("second re-enable = %+v, %v", again, err)
	}
}

func TestBackupDestinationStore_UpdateTouchesOnlyTheGivenFields(t *testing.T) {
	ctx := context.Background()
	st := api.NewBackupDestinationStore(openTestDB(t))
	created := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	if err := st.CreateDestination(ctx, backup.Destination{ID: "d", Name: "D", Type: backup.TypeLocal, Path: "/d", Enabled: false, CreatedAt: created, Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}); err != nil {
		t.Fatal(err)
	}
	alerted := created.Add(72 * time.Hour)
	if err := st.MarkStaleAlerted(ctx, "d", nil, alerted); err != nil {
		t.Fatal(err)
	}
	at := created.Add(200 * time.Hour)

	enabled := true
	if err := st.UpdateDestination(ctx, "d", backup.DestinationUpdate{Enabled: &enabled}, at); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDestination(ctx, "d")
	if !got.Enabled || got.Retention != (backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}) {
		t.Fatalf("after an enabled-only update: %+v, want the retention untouched", got)
	}
	if got.EnabledAt == nil || !got.EnabledAt.Equal(at) || got.StaleAlertedAt != nil {
		t.Fatalf("after a re-enable: %+v, want enabledAt set and the stale alert cleared", got)
	}

	if err := st.MarkStaleAlerted(ctx, "d", nil, alerted); err != nil {
		t.Fatal(err)
	}
	ret := backup.Retention{Daily: 1, Weekly: 1, Monthly: 1}
	if err := st.UpdateDestination(ctx, "d", backup.DestinationUpdate{Retention: &ret, Enabled: &enabled}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetDestination(ctx, "d")
	if got.Retention != ret || !got.EnabledAt.Equal(at) || got.StaleAlertedAt == nil {
		t.Fatalf("after enabling an enabled destination: %+v, want the clock and alert untouched", got)
	}

	if err := st.UpdateDestination(ctx, "nope", backup.DestinationUpdate{Enabled: &enabled}, at); !errors.Is(err, backup.ErrDestinationNotFound) {
		t.Fatalf("UpdateDestination(unknown) = %v, want ErrDestinationNotFound", err)
	}
	if err := st.DeleteDestination(ctx, "d"); err != nil {
		t.Fatalf("deleting a destination that has a re-enable record: %v", err)
	}
	if err := st.CreateDestination(ctx, backup.Destination{ID: "d", Name: "D", Type: backup.TypeLocal, Path: "/d", Enabled: true, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetDestination(ctx, "d"); got.EnabledAt != nil {
		t.Fatalf("a recreated destination inherited enabledAt %v", got.EnabledAt)
	}
}

// A pooled connection from store.DSN has foreign_keys off, so the schema's
// ON DELETE CASCADE cannot be what removes a re-enable record.
func TestBackupDestinationStore_DeleteLeavesNoReenableRecordWithForeignKeysOff(t *testing.T) {
	created := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	at := created.Add(200 * time.Hour)
	enabled := true

	newReenabled := func(t *testing.T, id string) (*api.BackupDestinationStore, *sql.DB) {
		t.Helper()
		ctx := context.Background()
		db := openTestDB(t)
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			t.Fatal(err)
		}
		var fk int
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 0 {
			t.Fatalf("foreign_keys = %d, %v; the test needs it off", fk, err)
		}
		st := api.NewBackupDestinationStore(db)
		d := backup.Destination{ID: id, Name: "D", Type: backup.TypeLocal, Path: "/d", Enabled: false, CreatedAt: created}
		if strings.HasPrefix(id, "external:") {
			label := strings.TrimPrefix(id, "external:")
			ext := store.ExternalDisk{Label: label, Device: "/dev/sde", Filesystem: "xfs", FSUUID: "uuid-" + label, Mountpoint: "/mnt/disks/" + label}
			if err := st.PutExternalDisk(ctx, ext, &d); err != nil {
				t.Fatal(err)
			}
		} else if err := st.CreateDestination(ctx, d); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateDestination(ctx, id, backup.DestinationUpdate{Enabled: &enabled}, at); err != nil {
			t.Fatal(err)
		}
		return st, db
	}
	assertClean := func(t *testing.T, db *sql.DB) {
		t.Helper()
		ctx := context.Background()
		var orphans int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_destination_enabled`).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Fatalf("%d backup_destination_enabled rows left after the destination was deleted", orphans)
		}
		rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			t.Fatal("foreign_key_check reports a violation after the delete")
		}
	}

	t.Run("DeleteDestination", func(t *testing.T) {
		st, db := newReenabled(t, "d")
		if err := st.DeleteDestination(context.Background(), "d"); err != nil {
			t.Fatal(err)
		}
		assertClean(t, db)
	})
	t.Run("DeleteDestination of an external disk's", func(t *testing.T) {
		st, db := newReenabled(t, "external:usb")
		if err := st.DeleteDestination(context.Background(), "external:usb"); err != nil {
			t.Fatal(err)
		}
		assertClean(t, db)
	})
	t.Run("SetExternalDestination off", func(t *testing.T) {
		st, db := newReenabled(t, "external:usb")
		if err := st.SetExternalDestination(context.Background(), "usb", nil); err != nil {
			t.Fatal(err)
		}
		assertClean(t, db)
	})
}
