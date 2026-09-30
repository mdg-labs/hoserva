package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mockFresh is whether the mock's installation has no array: the scenario
// with no array disks is the fresh install, where an import is the bare-metal
// restore of another installation's archive (doc 10 §1), as in production.
func (h *handler) mockFresh() bool {
	return mockArrayDisks(h.scenario) == nil
}

// mockLiveDatabase stands in for the live database a bare-metal restore reads
// this installation's schema version, machine key check and recipient from: an
// in-memory database migrated to the running schema, with those two rows
// seeded. The mock keeps no database of its own, so this is what makes an
// archive's schema decision and disk mapping the production code's own.
func mockLiveDatabase(ctx context.Context) (*sql.DB, error) {
	migrations, err := store.Load()
	if err != nil {
		return nil, err
	}
	snapshots, err := os.MkdirTemp("", "mockapi-live-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(snapshots) }()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: snapshots}).Apply(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, stmt := range []string{
		`INSERT INTO machine_key_check (id, check_value, created_at) VALUES (1, x'aa', '2026-09-01T00:00:00Z')`,
		`INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at) VALUES (1, 'age1mock', x'bb', x'cc', '2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func (h *handler) mockAttachedDisks() []disk.Disk {
	return mockInventoryAsDisks(mockDiskInventory(h.scenario))
}

// mockDiskMappingFromRequest reads the diskMapping field as production does.
func mockDiskMappingFromRequest(m apiv1.OptString) (*backup.DiskMapping, error) {
	doc, ok := m.Get()
	if !ok {
		return nil, nil
	}
	parsed, err := backup.ParseDiskMapping(doc)
	if err != nil {
		return nil, &mockError{code: "invalid_disk_mapping", statusCode: 400, message: err.Error()}
	}
	return &parsed, nil
}

// stageMockBareMetal stages the archive's database against the mock's live
// stand-in and refuses what production refuses: a newer archive, one that
// cannot be upgraded, an unreadable one. The caller discards the staged copy
// and closes the returned database.
func stageMockBareMetal(ctx context.Context, tree string) (*sql.DB, *backup.BareMetal, error) {
	live, err := mockLiveDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}
	bm, err := backup.StageBareMetal(ctx, live, tree)
	var (
		newer      *backup.NewerArchiveError
		upgrade    *backup.ArchiveUpgradeError
		unreadable *backup.UnreadableArchiveError
	)
	switch {
	case errors.As(err, &newer):
		err = &mockError{code: backup.RefusalNewerArchive, statusCode: 409, message: err.Error()}
	case errors.As(err, &upgrade):
		err = &mockError{code: backup.RefusalIncompatibleArchive, statusCode: 400, message: err.Error()}
	case errors.As(err, &unreadable):
		err = errInvalidArchive(err)
	}
	if err != nil {
		_ = live.Close()
		return nil, nil, err
	}
	return live, bm, nil
}

// mockBareMetalNotRestored confirms the mapping as production does (409
// disk_mapping_required, 409 disk_mapping_stale), opens the archive's secrets
// as production does after it (400 backup_passphrase_incorrect), and returns
// what the restore would not restore: the disks that did not match, the
// secrets cleared and the recipient kept, from the same
// backup.BareMetal.Apply, which only writes the staged copy.
func (h *handler) mockBareMetalNotRestored(ctx context.Context, tree string, mapping *backup.DiskMapping, passphrase apiv1.OptString) ([]backup.NotRestored, backup.SecretsOutcome, error) {
	live, bm, err := stageMockBareMetal(ctx, tree)
	if err != nil {
		return nil, backup.SecretsOutcome{}, err
	}
	defer func() { _ = live.Close() }()
	defer bm.Discard()
	mapped, err := bm.Confirm(h.mockAttachedDisks(), mapping)
	switch {
	case errors.Is(err, backup.ErrDiskMappingRequired):
		return nil, backup.SecretsOutcome{}, &mockError{code: "disk_mapping_required", statusCode: 409, message: err.Error()}
	case err != nil:
		var stale *backup.DiskMappingStaleError
		if errors.As(err, &stale) {
			return nil, backup.SecretsOutcome{}, &mockError{code: "disk_mapping_stale", statusCode: 409, message: err.Error()}
		}
		return nil, backup.SecretsOutcome{}, err
	}
	secrets, err := h.resolveMockSecrets(ctx, tree, passphrase)
	if err != nil {
		return nil, backup.SecretsOutcome{}, err
	}
	notRestored, err := bm.Apply(ctx, live, mapped, secrets, backup.FakeSecretCipher{})
	return notRestored, secrets, err
}

// mockBareMetalPreview is production's bareMetal block for the upload, with
// the schema-decision and mapping blockers that go with it. A nil block is
// what production answers for a newer archive.
func (h *handler) mockBareMetalPreview(ctx context.Context, tree string, secrets backup.SecretsOutcome) (*apiv1.ConfigImportBareMetal, []apiv1.ConfigImportBlocker, error) {
	live, err := mockLiveDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = live.Close() }()
	p, bm, err := backup.PreviewBareMetal(ctx, live, backup.Paths{}, tree, h.mockAttachedDisks(), secrets, backup.FakeSecretCipher{})
	var unreadable *backup.UnreadableArchiveError
	if errors.As(err, &unreadable) {
		return nil, nil, errInvalidArchive(err)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("previewing the import: %w", err)
	}
	blockers := make([]apiv1.ConfigImportBlocker, len(p.Blockers))
	for i, b := range p.Blockers {
		blockers[i] = apiv1.ConfigImportBlocker{Code: apiv1.ConfigImportBlockerCode(b.Code), Message: b.Message}
	}
	if bm == nil {
		return nil, blockers, nil
	}
	out := apiv1.ConfigImportBareMetal{SchemaUpgrade: bm.SchemaUpgrade, Disks: make([]apiv1.ConfigImportDisk, len(bm.Disks))}
	for i, d := range bm.Disks {
		r := d.Recorded
		e := apiv1.ConfigImportDisk{
			Name: d.Name(), Role: apiv1.ArrayDiskRole(r.Role), RoleIndex: int64(r.RoleIndex), Mountpoint: r.Mountpoint,
			FsUuid: r.FSUUID, WeakIdentity: r.WeakIdentity, State: apiv1.ConfigImportDiskState(d.State),
		}
		if r.WWN != "" {
			e.Wwn = apiv1.NewOptString(r.WWN)
		}
		if r.Serial != "" {
			e.Serial = apiv1.NewOptString(r.Serial)
		}
		if r.ByIDName != "" {
			e.ByIdName = apiv1.NewOptString(r.ByIDName)
		}
		if r.SizeSet {
			e.SizeBytes = apiv1.NewOptInt64(r.Size)
		}
		if d.Attached != nil {
			e.Device = apiv1.NewOptString(d.Attached.Device)
		}
		out.Disks[i] = e
	}
	m := backup.MatchedMapping(bm.Disks)
	out.DiskMapping = apiv1.ConfigImportDiskMapping{Disks: make([]apiv1.ConfigImportDiskMappingEntry, len(m.Disks))}
	for i, e := range m.Disks {
		out.DiskMapping.Disks[i] = apiv1.ConfigImportDiskMappingEntry{Role: apiv1.ArrayDiskRole(e.Role), RoleIndex: int64(e.RoleIndex), Device: e.Device}
	}
	return &out, blockers, nil
}
