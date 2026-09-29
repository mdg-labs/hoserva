package api_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
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
	if err := st.MarkStaleAlerted(ctx, "dest-1", alertedAt); err != nil {
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
