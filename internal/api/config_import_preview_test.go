package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

func previewReq(archive []byte) *apiv1.PreviewConfigImportReq {
	return &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
}

func previewNames(cs []apiv1.ConfigImportChange) []string {
	names := []string{}
	for _, c := range cs {
		names = append(names, string(c.Kind)+"="+c.Name)
	}
	return names
}

func previewGroup(t *testing.T, p *apiv1.ConfigImportPreview, c apiv1.ConfigImportGroupCategory) apiv1.ConfigImportGroup {
	t.Helper()
	for _, g := range p.Groups {
		if g.Category == c {
			return g
		}
	}
	t.Fatalf("no %s group in the preview", c)
	return apiv1.ConfigImportGroup{}
}

// dumpDatabase reads every row of every table, so a test can prove a call
// changed none of them.
func dumpDatabase(t *testing.T, db *sql.DB) string {
	t.Helper()
	var tables []string
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning table name: %v", err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	var b strings.Builder
	for _, n := range tables {
		rs, err := db.Query(fmt.Sprintf(`SELECT * FROM "%s"`, n))
		if err != nil {
			t.Fatalf("reading %s: %v", n, err)
		}
		cols, _ := rs.Columns()
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatalf("scanning %s: %v", n, err)
			}
			fmt.Fprintf(&b, "%s: %v\n", n, vals)
		}
		_ = rs.Close()
	}
	return b.String()
}

func setTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
	}
}

func TestPreviewConfigImport_ReportsWhatAnImportWouldChange(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)
	stamp := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	h.Backup.Now = func() time.Time { return stamp }
	h.Backup.Hostname = "nas"
	h.Backup.Version = "1.2.3"
	insertSentinelShare(t, db, "keep")
	insertSentinelShare(t, db, "changed")
	insertSentinelShare(t, db, "deleted-since")
	archive := exportBytes(t, h)

	execAll(t, db,
		`DELETE FROM shares WHERE name = 'deleted-since'`,
		`UPDATE shares SET create_policy = 'lfs' WHERE name = 'changed'`,
	)
	insertSentinelShare(t, db, "created-since")

	p, err := h.PreviewConfigImport(ctx, previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	if p.Archive.Host != "nas" || p.Archive.HoservaVersion != "1.2.3" || !p.Archive.Timestamp.Equal(stamp) {
		t.Fatalf("archive = %+v, want the manifest's host, version and time", p.Archive)
	}
	if p.Archive.SchemaVersion == "" || p.Archive.SchemaVersion != p.LiveSchemaVersion {
		t.Fatalf("schema versions = %q / %q, want the same non-empty version", p.Archive.SchemaVersion, p.LiveSchemaVersion)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers = %+v, want none", p.Blockers)
	}
	var categories []apiv1.ConfigImportGroupCategory
	for _, g := range p.Groups {
		categories = append(categories, g.Category)
	}
	if want := (apiv1.ConfigImportGroupCategory("")).AllValues(); !slices.Equal(categories, want) {
		t.Fatalf("categories = %v, want %v", categories, want)
	}
	shares := previewGroup(t, p, apiv1.ConfigImportGroupCategoryShares)
	if got := previewNames(shares.Added); !slices.Equal(got, []string{"share=deleted-since"}) {
		t.Fatalf("added = %v, want the share the import would bring back", got)
	}
	if got := previewNames(shares.Changed); !slices.Equal(got, []string{"share=changed"}) {
		t.Fatalf("changed = %v", got)
	}
	if got := previewNames(shares.Removed); !slices.Equal(got, []string{"share=created-since"}) {
		t.Fatalf("removed = %v, want the share created after the archive", got)
	}
	for _, g := range p.Groups {
		if g.Category != apiv1.ConfigImportGroupCategoryShares && len(g.Added)+len(g.Changed)+len(g.Removed) != 0 {
			t.Fatalf("%s reports %+v, want no changes outside shares", g.Category, g)
		}
	}
	if len(p.Notes) != 1 || p.Notes[0].Code != apiv1.ConfigImportNoteCodeSessionsReplaced {
		t.Fatalf("notes = %+v, want the sessions note", p.Notes)
	}
}

// The installation's settings an import would revert are listed under
// system settings by kind, and a passphrase never appears as a value.
func TestPreviewConfigImport_ListsSettingsAnImportWouldRevert(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	execAll(t, db,
		`INSERT INTO schema_info (id, installation_id, created_at, hostname, timezone, backup_passphrase, update_channel, update_check_enabled)
			VALUES (1, 'inst', 't', 'nas-old', 'UTC', x'aa', 'stable', 1)`,
		`INSERT INTO host_config (kind, decision, facts, applied_at) VALUES ('samba', 'import', '{}', 't')`,
		`INSERT INTO external_disks (label, device, filesystem, fs_uuid, weak_identity, mountpoint, backup_destination)
			VALUES ('usb-old', '/dev/sdx', 'ext4', 'u1', 0, '/mnt/disks/usb-old', 0)`,
	)
	archive := exportBytes(t, h)
	execAll(t, db,
		`UPDATE schema_info SET hostname = 'nas-new', timezone = 'Europe/Vienna', backup_passphrase = x'bb', update_channel = 'beta', update_check_enabled = 0`,
		`UPDATE host_config SET decision = 'leave'`,
		`DELETE FROM external_disks`,
		`INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, 1, 0, 't')`,
	)

	p, err := h.PreviewConfigImport(context.Background(), previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers = %+v, want none", p.Blockers)
	}
	system := previewGroup(t, p, apiv1.ConfigImportGroupCategorySystem)
	wantChanged := []string{"backup_passphrase=", "host_config=samba", "hostname=", "timezone=", "update_channel=", "update_check="}
	got := previewNames(system.Changed)
	slices.Sort(got)
	if !slices.Equal(got, wantChanged) {
		t.Fatalf("changed = %v, want %v", got, wantChanged)
	}
	if got := previewNames(system.Added); !slices.Equal(got, []string{"external_disk=usb-old"}) {
		t.Fatalf("added = %v, want the external disk the import would bring back", got)
	}
	if got := previewNames(system.Removed); !slices.Equal(got, []string{"array_state="}) {
		t.Fatalf("removed = %v, want the maintenance state the import would clear", got)
	}
}

func TestPreviewConfigImport_UnchangedArchiveReportsNoChanges(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	insertSentinelShare(t, db, "media")
	archive := exportBytes(t, h)

	p, err := h.PreviewConfigImport(context.Background(), previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	for _, g := range p.Groups {
		if len(g.Added)+len(g.Changed)+len(g.Removed) != 0 {
			t.Fatalf("%s reports %+v for an archive of the running configuration", g.Category, g)
		}
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers = %+v", p.Blockers)
	}
}

// Every refusal ImportConfig has that depends on the archive appears in the
// preview as a blocker with the code and message the import returns, since
// both run backup.CheckImport.
func TestPreviewConfigImport_EveryImportRefusalIsABlocker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   string
		status int
		build  func(t *testing.T) (*Handler, *sql.DB, []byte)
	}{
		{"incompatible_archive", "incompatible_archive", 400, func(t *testing.T) (*Handler, *sql.DB, []byte) {
			h, db, _ := newImportTestHandler(t)
			return h, db, editArchiveDB(t, exportBytes(t, h), func(adb *sql.DB) {
				execAll(t, adb, `DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`)
			})
		}},
		{"archive_other_installation", "archive_other_installation", 409, func(t *testing.T) (*Handler, *sql.DB, []byte) {
			h, db, _ := newImportTestHandler(t)
			other, otherDB, _ := newImportTestHandler(t)
			seedMachineKeyCheck(t, otherDB, []byte("installation-b-check-value"))
			return h, db, exportBytes(t, other)
		}},
		{"archive_array_mismatch", "archive_array_mismatch", 409, func(t *testing.T) (*Handler, *sql.DB, []byte) {
			h, db, _ := newImportTestHandler(t)
			archive := exportBytes(t, h)
			seedArray(t, db)
			return h, db, archive
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, archive := tc.build(t)

			p, err := h.PreviewConfigImport(context.Background(), previewReq(archive))
			if err != nil {
				t.Fatalf("PreviewConfigImport: %v", err)
			}
			importErr := h.ImportConfig(context.Background(), importReq(archive))
			ae, ok := importErr.(*apiError)
			if !ok || ae.code != tc.code || ae.statusCode != tc.status {
				t.Fatalf("ImportConfig err = %v (%T), want (%d, %s)", importErr, importErr, tc.status, tc.code)
			}
			if len(p.Blockers) != 1 {
				t.Fatalf("blockers = %+v, want the import's one refusal", p.Blockers)
			}
			if string(p.Blockers[0].Code) != ae.code || p.Blockers[0].Message != ae.message {
				t.Fatalf("blocker = (%s, %q), import refusal = (%s, %q)", p.Blockers[0].Code, p.Blockers[0].Message, ae.code, ae.message)
			}
		})
	}
}

// The preview changes nothing: no database row, no pre-import archive, no
// job or restore hold, no file left in the temporary directory.
func TestPreviewConfigImport_WritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (*Handler, *sql.DB, []byte)
	}{
		{"an importable archive", func(t *testing.T) (*Handler, *sql.DB, []byte) {
			h, db, _ := newImportTestHandler(t)
			insertSentinelShare(t, db, "gone-since")
			archive := exportBytes(t, h)
			execAll(t, db, `DELETE FROM shares`)
			return h, db, archive
		}},
		{"a refused archive", func(t *testing.T) (*Handler, *sql.DB, []byte) {
			h, db, _ := newImportTestHandler(t)
			other, otherDB, _ := newImportTestHandler(t)
			seedMachineKeyCheck(t, otherDB, []byte("installation-b-check-value"))
			insertSentinelShare(t, db, "live")
			return h, db, exportBytes(t, other)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, db, archive := tc.build(t)
			destDir := t.TempDir()
			h.Backup.Destinations = []backup.Destination{{
				ID: "local", Path: destDir, Enabled: true,
				Retention: backup.Retention{Daily: backup.DefaultRetentionDaily, Weekly: backup.DefaultRetentionWeekly, Monthly: backup.DefaultRetentionMonthly},
			}}
			backups := 0
			h.Backup.Now = func() time.Time {
				backups++
				return time.Now().UTC()
			}
			before := dumpDatabase(t, db)
			tmp := setTempDir(t)

			if _, err := h.PreviewConfigImport(ctx, previewReq(archive)); err != nil {
				t.Fatalf("PreviewConfigImport: %v", err)
			}

			if backups != 0 {
				t.Fatalf("a backup ran %d time(s) during a preview", backups)
			}
			assertEmptyDir(t, destDir)
			assertEmptyDir(t, tmp)
			if after := dumpDatabase(t, db); after != before {
				t.Fatalf("the preview changed the live database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			release, err := h.Scheduler.BeginDatabaseRestore(ctx)
			if err != nil {
				t.Fatalf("the restore hold is held after a preview: %v", err)
			}
			release()
			active, err := h.Store.ListActive(ctx)
			if err != nil || len(active) != 0 {
				t.Fatalf("active jobs after a preview = %v (err %v), want none", active, err)
			}
		})
	}
}

// A running job refuses an import but is not a property of the archive, so
// the preview still answers.
func TestPreviewConfigImport_AnswersWhileAJobRuns(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)
	if err := job.NewStore(db).Create(context.Background(), &job.Job{
		ID: uuid.NewString(), Type: job.TypeSync, Class: job.ClassParity,
		Status: job.StatusRunning, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seeding an active job: %v", err)
	}
	if _, err := h.PreviewConfigImport(context.Background(), previewReq(archive)); err != nil {
		t.Fatalf("PreviewConfigImport with a job running: %v", err)
	}
}

// The preview refuses an upload the way importConfig does before any
// comparison, and leaves nothing in the temporary directory on any path.
func TestPreviewConfigImport_RefusesWhatImportConfigRefuses(t *testing.T) {
	h, _, _ := newImportTestHandler(t)
	good := exportBytes(t, h)

	for _, tc := range []struct {
		name   string
		body   func(t *testing.T) []byte
		limit  int64
		status int
		code   string
	}{
		{"not an archive", func(*testing.T) []byte { return []byte("not a tar.zst archive") }, 0, 400, "invalid_archive"},
		{"truncated", func(*testing.T) []byte { return good[:len(good)/2] }, 0, 400, "invalid_archive"},
		{"checksum mismatch", func(t *testing.T) []byte {
			return rewriteArchive(t, good, func(staging string) {
				if err := tamperStateDB(t, filepath.Join(staging, "state.db")); err != nil {
					t.Fatalf("tampering state.db: %v", err)
				}
			})
		}, 0, 400, "invalid_archive"},
		{"no state.db", func(t *testing.T) []byte {
			return rewriteArchive(t, good, func(staging string) {
				if err := os.Remove(filepath.Join(staging, "state.db")); err != nil {
					t.Fatalf("removing state.db: %v", err)
				}
			})
		}, 0, 400, "invalid_archive"},
		{"unlisted file", func(t *testing.T) []byte {
			return rewriteArchive(t, good, func(staging string) {
				if err := os.WriteFile(filepath.Join(staging, "extra.txt"), []byte("x"), 0o600); err != nil {
					t.Fatalf("writing extra file: %v", err)
				}
			})
		}, 0, 400, "invalid_archive"},
		{"too large", func(*testing.T) []byte { return good }, 100, 413, "archive_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			if tc.limit != 0 {
				old := maxConfigArchiveBytes
				maxConfigArchiveBytes = tc.limit
				t.Cleanup(func() { maxConfigArchiveBytes = old })
			}
			tmp := setTempDir(t)

			_, previewErr := h.PreviewConfigImport(context.Background(), previewReq(body))
			importErr := h.ImportConfig(context.Background(), importReq(body))

			for name, err := range map[string]error{"preview": previewErr, "import": importErr} {
				ae, ok := err.(*apiError)
				if !ok || ae.statusCode != tc.status || ae.code != tc.code {
					t.Fatalf("%s err = %v (%T), want (%d, %s)", name, err, err, tc.status, tc.code)
				}
			}
			assertEmptyDir(t, tmp)
		})
	}
}

func TestPreviewConfigImport_NotConfigured(t *testing.T) {
	for name, h := range map[string]*Handler{
		"no backup service": {},
		"no database":       {Backup: &backup.Service{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.PreviewConfigImport(context.Background(), previewReq([]byte("x")))
			ae, ok := err.(*apiError)
			if !ok || ae.statusCode != 501 || ae.code != "not_configured" {
				t.Fatalf("err = %v (%T), want (501, not_configured)", err, err)
			}
		})
	}
}

// Every kind and category the comparison can report is one the API schema
// names, and the schema names no others.
func TestPreviewConfigImport_KindsMatchTheSchema(t *testing.T) {
	var schema []string
	for _, k := range (apiv1.ConfigImportChangeKind("")).AllValues() {
		schema = append(schema, string(k))
	}
	got := backup.ImportChangeKinds()
	slices.Sort(schema)
	slices.Sort(got)
	if !slices.Equal(got, schema) {
		t.Fatalf("kinds the comparison reports = %v, schema = %v", got, schema)
	}
}
