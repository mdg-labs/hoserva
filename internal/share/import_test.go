package share

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestImportFromHost_InsertsSambaAndNFS(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	samba, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parsers", "host_smb.conf"))
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parsers", "host_exports"))
	if err != nil {
		t.Fatal(err)
	}

	inserted, err := svc.ImportFromHost(ctx, samba, nfs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 3 {
		t.Fatalf("inserted = %v, want media, homes, backup", inserted)
	}

	media, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if media.CacheMode != pool.ArrayOnly || media.CreatePolicy != pool.DefaultCreatePolicy {
		t.Fatalf("media defaults = %+v", media)
	}
	if !media.SMB.Enabled || media.SMB.ReadOnly || !media.SMB.Browseable {
		t.Fatalf("media SMB = %+v", media.SMB)
	}
	if !media.NFS.Enabled || len(media.NFS.Hosts) != 1 || media.NFS.Hosts[0] != "192.168.1.0/24" {
		t.Fatalf("media NFS = %+v", media.NFS)
	}

	backup, err := svc.Get(ctx, "backup")
	if err != nil {
		t.Fatal(err)
	}
	if backup.SMB.Enabled || !backup.NFS.Enabled || backup.NFS.Hosts[0] != "*" {
		t.Fatalf("backup = %+v", backup)
	}
}

func TestImportFromHost_SkipsDuplicatesAndInvalidNames(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	if err := svc.Shares.Insert(ctx, store.Share{
		Name: "media", CacheMode: "array-only", CreatePolicy: "mspmfs",
		CreatedAt: now, UpdatedAt: now, NFSSquash: "root_squash",
	}); err != nil {
		t.Fatal(err)
	}

	samba := []byte("[media]\npath = /x\n\n[bad name]\npath = /y\n\n[ok]\nbrowseable = no\n")
	nfs := []byte("/export/media *(ro,sync)\n/export/.hidden *(ro,sync)\n")
	inserted, err := svc.ImportFromHost(ctx, samba, nfs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 1 || inserted[0] != "ok" {
		t.Fatalf("inserted = %v, want [ok]", inserted)
	}
	media, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if media.SMB.Enabled || media.NFS.Enabled {
		t.Fatalf("pre-existing media should be untouched: %+v", media)
	}
}

func TestImportFromHost_NilSourcesAreNoOp(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	inserted, err := svc.ImportFromHost(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 0 {
		t.Fatalf("inserted = %v", inserted)
	}
	rows, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d", len(rows))
	}
}
