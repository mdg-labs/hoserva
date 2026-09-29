package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

func openMigratedDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(path))
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO machine_key_check (id, check_value, created_at) VALUES (1, x'aa', 't')`); err != nil {
		t.Fatalf("seeding machine_key_check: %v", err)
	}
	return db
}

// tableFixture makes rows of one configTable. row(name, v) is an INSERT for
// the row keyed by name, whose content depends on v: two rows with the same
// name and different v differ in a compared column. names are the keys for
// the row present on both sides, the one that changes, the archive-only one
// and the live-only one. A singleton has one row, so its scenarios are
// separate.
type tableFixture struct {
	table string
	// kind picks the configTable when a table is several kinds.
	kind      string
	names     [4]string
	singleton bool
	noChange  bool
	label     func(name string) string
	row       func(name string, v int) string
	// parents are the rows a foreign key needs before row(name, ...) can be
	// inserted; they are identical on both sides.
	parents func(name string) []string
}

func (f tableFixture) insert(t *testing.T, db *sql.DB, name string, v int) {
	t.Helper()
	if f.parents != nil {
		mustExec(t, db, f.parents(name)...)
	}
	mustExec(t, db, f.row(name, v))
}

const seedShare = `INSERT OR IGNORE INTO shares (name, cache_mode, create_policy, smb_enabled, smb_guest, smb_read_only, smb_browseable, smb_recycle, smb_time_machine, created_at, updated_at)
	VALUES ('s1', 'array-only', 'mfs', 1, 0, 0, 1, 0, 0, 't', 't')`

func seedUser(id, name string) string {
	return fmt.Sprintf(`INSERT OR IGNORE INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('%s', '%s', 'h', 'viewer', 0, 't')`, id, name)
}

func seedGroup(id, name string) string {
	return fmt.Sprintf(`INSERT OR IGNORE INTO user_groups (id, name, created_at) VALUES ('%s', '%s', 't')`, id, name)
}

const seedChannel = `INSERT OR IGNORE INTO notify_channels (id, name, "type", enabled, config, created_at, updated_at) VALUES ('c1', 'chan', 'ntfy', 1, '{}', 't', 't')`

var defaultNames = [4]string{"keep", "chg", "only-arc", "only-live"}

func plain(name string) string { return name }

func tableFixtures() []tableFixture {
	const ts = "2026-01-01T00:00:00Z"
	pick := func(v int, vals ...string) string { return vals[v%len(vals)] }
	return []tableFixture{
		{table: "shares", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO shares (name, cache_mode, create_policy, smb_enabled, smb_guest, smb_read_only, smb_browseable, smb_recycle, smb_time_machine, created_at, updated_at)
				VALUES ('%s', 'array-only', '%s', 1, 0, 0, 1, 0, 0, '%s', '%s')`, n, pick(v, "mfs", "lfs"), ts, ts)
		}},
		{table: "share_user_permissions", names: defaultNames, label: func(n string) string { return "s1 / " + n },
			parents: func(n string) []string { return []string{seedShare, seedUser("id-"+n, n)} },
			row: func(n string, v int) string {
				return fmt.Sprintf(`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES ('s1', 'id-%s', '%s')`, n, pick(v, "read-only", "read-write"))
			}},
		{table: "share_group_permissions", names: defaultNames, label: func(n string) string { return "s1 / " + n },
			parents: func(n string) []string { return []string{seedShare, seedGroup("id-"+n, n)} },
			row: func(n string, v int) string {
				return fmt.Sprintf(`INSERT INTO share_group_permissions (share_name, group_id, access) VALUES ('s1', 'id-%s', '%s')`, n, pick(v, "read-only", "read-write"))
			}},

		{table: "users", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('id-%s', '%s', 'hash%d', 'viewer', 0, '%s')`, n, n, v, ts)
		}},
		{table: "user_groups", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO user_groups (id, name, created_at) VALUES ('id-%s', '%s', '%s%d')`, n, n, ts, v)
		}},
		{table: "user_group_members", names: defaultNames, noChange: true, label: func(n string) string { return n + " / u1" },
			parents: func(n string) []string { return []string{seedGroup("id-"+n, n), seedUser("u1", "u1")} },
			row: func(n string, v int) string {
				return fmt.Sprintf(`INSERT INTO user_group_members (group_id, user_id) VALUES ('id-%s', 'u1')`, n)
			}},
		{table: "api_tokens", names: defaultNames, label: func(n string) string { return "u1 / " + n },
			parents: func(string) []string { return []string{seedUser("u1", "u1")} },
			row: func(n string, v int) string {
				return fmt.Sprintf(`INSERT INTO api_tokens (token_hash, user_id, name, role, created_at) VALUES ('hash-%s', 'u1', '%s', '%s', '%s')`, n, n, pick(v, "viewer", "admin"), ts)
			}},

		{table: "schedule_chain", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO schedule_chain (id, start_time, weekly_scrub_day, mover_enabled, diff_guard_enabled, sync_enabled, scrub_enabled, config_backup_enabled, updated_at)
				VALUES (1, '03:00', %d, 1, 1, 1, 1, 1, '%s')`, v, ts)
		}},
		{table: "schedule_jobs", names: [4]string{"smart_self_test", "appdata_backup", "restore_drill", "container_update_check"}, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO schedule_jobs (job_id, enabled, frequency, start_time, updated_at) VALUES ('%s', 1, '%s', '04:00', '%s')`, n, pick(v, "daily", "weekly"), ts)
		}},

		{table: "notify_channels", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO notify_channels (id, name, "type", enabled, config, created_at, updated_at) VALUES ('id-%s', '%s', 'ntfy', %d, '{}', '%s', '%s')`, n, n, v, ts, ts)
		}},
		{table: "notify_routes", names: defaultNames, noChange: true, label: func(n string) string { return n + " / chan" },
			parents: func(string) []string { return []string{seedChannel} },
			row: func(n string, v int) string {
				return fmt.Sprintf(`INSERT INTO notify_routes (event_type, channel_id) VALUES ('%s', 'c1')`, n)
			}},
		{table: "notify_event_severity", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO notify_event_severity (event_type, severity) VALUES ('%s', '%s')`, n, pick(v, "info", "warning"))
		}},
		{table: "notify_quiet_hours", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO notify_quiet_hours (id, enabled, start_time, end_time, updated_at) VALUES (1, 1, '2%d:00', '07:00', '%s')`, v, ts)
		}},

		{table: "backup_destinations", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO backup_destinations (id, name, type, path, options, secrets, enabled, encrypt, retention_daily, retention_weekly, retention_monthly, created_at)
				VALUES ('id-%s', '%s', 'local', '/p%d', '{}', x'', 1, 0, 7, 4, 6, '%s')`, n, n, v, ts)
		}},
		{table: "appdata_backup_containers", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO appdata_backup_containers (container, stop, included, updated_at) VALUES ('%s', %d, 1, '%s')`, n, v, ts)
		}},

		{table: "backup_recipient", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at) VALUES (1, 'age1recipient%d', x'01', x'02', '%s')`, v, ts)
		}},

		{table: "acme_config", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO acme_config (id, domain, provider, provider_config, dns_secret, account_key, enabled, updated_at)
				VALUES (1, 'nas%d.example.org', 'cloudflare', '{}', x'01', x'02', 1, '%s')`, v, ts)
		}},
		{table: "ups_config", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO ups_config (id, connection, driver, port, monitor_password, network_host, network_port, network_ups_name, network_username, network_password, low_battery_percent, runtime_seconds, updated_at)
				VALUES (1, 'usb', 'usbhid-ups', 'auto', x'01', '', 0, '', '', x'', %d, 300, '%s')`, 20+v, ts)
		}},
		{table: "array_settings", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO array_settings (id, create_policy, min_free_space, created_at) VALUES (1, '%s', '50G', '%s')`, pick(v, "mspmfs", "mfs"), ts)
		}},
		{table: "array_maintenance", kind: "array_state", singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
			return fmt.Sprintf(`INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, %d, 0, '%s')`, v, ts)
		}},
		{table: "host_config", names: [4]string{"fstab", "samba", "nfs", "docker_images"}, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO host_config (kind, decision, facts, applied_at) VALUES ('%s', '%s', '{}', '%s')`, n, pick(v, "import", "leave"), ts)
		}},
		{table: "external_disks", names: defaultNames, label: plain, row: func(n string, v int) string {
			return fmt.Sprintf(`INSERT INTO external_disks (label, device, filesystem, fs_uuid, weak_identity, mountpoint, backup_destination)
				VALUES ('%s', '/dev/x-%s', 'ext4', 'uuid-%s', 0, '/mnt/disks/%s', %d)`, n, n, n, n, v)
		}},
		schemaInfoFixture("hostname", "hostname", func(v int) string { return pick(v, "'nas-a'", "'nas-b'") }),
		schemaInfoFixture("timezone", "timezone", func(v int) string { return pick(v, "'Europe/Vienna'", "'UTC'") }),
		schemaInfoFixture("backup_passphrase", "backup_passphrase", func(v int) string { return pick(v, "x'01'", "x'02'") }),
		schemaInfoFixture("update_channel", "update_channel", func(v int) string { return pick(v, "'stable'", "'beta'") }),
		schemaInfoFixture("update_check", "update_check_enabled", func(v int) string { return pick(v, "1", "0") }),
	}
}

// schemaInfoFixture is a schema_info setting: one row holds them all, so a
// fixture sets just its own column and leaves the others at their defaults.
func schemaInfoFixture(kind, column string, value func(v int) string) tableFixture {
	return tableFixture{table: "schema_info", kind: kind, singleton: true, label: func(string) string { return "" }, row: func(_ string, v int) string {
		return fmt.Sprintf(`INSERT INTO schema_info (id, installation_id, created_at, %[1]s) VALUES (1, 'inst', 't', %[2]s)
			ON CONFLICT (id) DO UPDATE SET %[1]s = excluded.%[1]s`, column, value(v))
	}}
}

func fixtureFor(t *testing.T, table string) tableFixture {
	t.Helper()
	for _, f := range tableFixtures() {
		if f.table == table {
			return f
		}
	}
	t.Fatalf("no fixture for %s", table)
	return tableFixture{}
}

func mustExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func groupByCategory(t *testing.T, groups []ImportGroup, category string) ImportGroup {
	t.Helper()
	for _, g := range groups {
		if g.Category == category {
			return g
		}
	}
	t.Fatalf("no %s group in %+v", category, groups)
	return ImportGroup{}
}

// ofKind keeps the changes of one kind: a fixture's parent rows, which only
// one side may hold, are reported under their own kinds.
func ofKind(cs []ImportChange, kind string) []ImportChange {
	out := []ImportChange{}
	for _, c := range cs {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func changesOf(kind string, names ...string) []ImportChange {
	out := make([]ImportChange, len(names))
	for i, n := range names {
		out[i] = ImportChange{Kind: kind, Name: n}
	}
	return out
}

func (f tableFixture) entry(t *testing.T) configTable {
	t.Helper()
	for _, c := range configTables {
		if c.table == f.table && (f.kind == "" || c.kind == f.kind) {
			return c
		}
	}
	t.Fatalf("no configTable for %s %s", f.table, f.kind)
	return configTable{}
}

func TestFixturesCoverEveryConfigTable(t *testing.T) {
	var got, want []string
	for _, f := range tableFixtures() {
		got = append(got, f.entry(t).kind)
	}
	for _, c := range configTables {
		want = append(want, c.kind)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixtures cover %v, configTables are %v", got, want)
	}
}

func sqliteNames(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// An import replaces the whole database, so every table in it is either
// compared or classified as one a preview does not list, with the reason:
// a table added to the schema fails here until it is one or the other.
func TestEveryTableIsComparedOrClassified(t *testing.T) {
	db := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	tables := sqliteNames(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)

	compared := map[string]bool{}
	for _, c := range configTables {
		compared[c.table] = true
	}
	inSchema := map[string]bool{}
	for _, n := range tables {
		inSchema[n] = true
		reason, classified := uncomparedTables[n]
		switch {
		case compared[n] && classified:
			t.Errorf("table %s is both compared and classified as %q", n, reason)
		case !compared[n] && !classified:
			t.Errorf("table %s is neither compared nor classified in uncomparedTables", n)
		case classified && reason == "":
			t.Errorf("table %s is classified without a reason", n)
		}
	}
	for n := range uncomparedTables {
		if !inSchema[n] {
			t.Errorf("uncomparedTables names %s, which is not a table", n)
		}
	}
	for n := range compared {
		if !inSchema[n] {
			t.Errorf("configTables names %s, which is not a table", n)
		}
	}
}

// schema_info holds settings a preview reports and columns it does not: each
// column is in one kind's only list or in uncomparedColumns, with a reason.
func TestEverySchemaInfoColumnIsComparedOrClassified(t *testing.T) {
	db := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	columns := sqliteNames(t, db, `SELECT name FROM pragma_table_info('schema_info')`)

	compared := map[string]bool{}
	for _, c := range configTables {
		if c.table != "schema_info" {
			continue
		}
		if len(c.only) == 0 {
			t.Fatalf("schema_info kind %s compares every column", c.kind)
		}
		for _, col := range c.only {
			compared[col] = true
		}
	}
	inSchema := map[string]bool{}
	for _, col := range columns {
		inSchema[col] = true
		reason, classified := uncomparedColumns["schema_info."+col]
		switch {
		case compared[col] && classified:
			t.Errorf("schema_info.%s is both compared and classified as %q", col, reason)
		case !compared[col] && !classified:
			t.Errorf("schema_info.%s is neither compared nor classified in uncomparedColumns", col)
		case classified && reason == "":
			t.Errorf("schema_info.%s is classified without a reason", col)
		}
	}
	for key := range uncomparedColumns {
		if !inSchema[strings.TrimPrefix(key, "schema_info.")] {
			t.Errorf("uncomparedColumns names %s, which is not a column", key)
		}
	}
	for col := range compared {
		if !inSchema[col] {
			t.Errorf("a schema_info kind compares %s, which is not a column", col)
		}
	}
}

// Every table a preview compares reports one added, one changed and one
// removed item under its own category and kind, by the name a user knows,
// and leaves an identical row unreported.
func TestPreviewImport_ReportsAnAddedChangedAndRemovedItemPerTable(t *testing.T) {
	ctx := context.Background()
	for _, f := range tableFixtures() {
		entry := f.entry(t)
		category, kind := entry.category, entry.kind
		if f.singleton {
			for _, sc := range []struct {
				name              string
				live, arc         *int
				added, changed, r bool
			}{
				{"added", nil, ptr(0), true, false, false},
				{"removed", ptr(0), nil, false, false, true},
				{"changed", ptr(0), ptr(1), false, true, false},
				{"unchanged", ptr(0), ptr(0), false, false, false},
			} {
				t.Run(kind+" "+sc.name, func(t *testing.T) {
					live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
					arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
					if sc.live != nil {
						f.insert(t, live, "", *sc.live)
					}
					if sc.arc != nil {
						f.insert(t, arc, "", *sc.arc)
					}
					groups, err := diffConfig(ctx, live, arc)
					if err != nil {
						t.Fatalf("diffConfig: %v", err)
					}
					g := groupByCategory(t, groups, category)
					g.Added, g.Changed, g.Removed = ofKind(g.Added, kind), ofKind(g.Changed, kind), ofKind(g.Removed, kind)
					want := changesOf(kind, "")
					check := func(field string, got []ImportChange, expected bool) {
						if expected && !reflect.DeepEqual(got, want) {
							t.Errorf("%s = %v, want %v", field, got, want)
						}
						if !expected && len(got) != 0 {
							t.Errorf("%s = %v, want none", field, got)
						}
					}
					check("Added", g.Added, sc.added)
					check("Changed", g.Changed, sc.changed)
					check("Removed", g.Removed, sc.r)
				})
			}
			continue
		}
		t.Run(f.table, func(t *testing.T) {
			live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
			arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
			keep, chg, onlyArc, onlyLive := f.names[0], f.names[1], f.names[2], f.names[3]
			f.insert(t, live, keep, 0)
			f.insert(t, live, chg, 0)
			f.insert(t, live, onlyLive, 0)
			f.insert(t, arc, keep, 0)
			f.insert(t, arc, chg, 1)
			f.insert(t, arc, onlyArc, 0)
			groups, err := diffConfig(ctx, live, arc)
			if err != nil {
				t.Fatalf("diffConfig: %v", err)
			}
			g := groupByCategory(t, groups, category)
			g.Added, g.Changed, g.Removed = ofKind(g.Added, kind), ofKind(g.Changed, kind), ofKind(g.Removed, kind)
			if want := changesOf(kind, f.label(onlyArc)); !reflect.DeepEqual(g.Added, want) {
				t.Errorf("Added = %v, want %v", g.Added, want)
			}
			if want := changesOf(kind, f.label(onlyLive)); !reflect.DeepEqual(g.Removed, want) {
				t.Errorf("Removed = %v, want %v", g.Removed, want)
			}
			want := changesOf(kind, f.label(chg))
			if f.noChange {
				want = []ImportChange{}
			}
			if !reflect.DeepEqual(g.Changed, want) {
				t.Errorf("Changed = %v, want %v", g.Changed, want)
			}
		})
	}
}

func ptr(v int) *int { return &v }

// A table whose rows are all key has no column that can differ, so its
// "changed" row is identical on both sides and is not reported.
func TestPreviewImport_KeyOnlyTablesReportNoChanges(t *testing.T) {
	ctx := context.Background()
	for _, f := range tableFixtures() {
		if !f.noChange {
			continue
		}
		live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
		arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
		f.insert(t, live, "same", 0)
		f.insert(t, arc, "same", 0)
		groups, err := diffConfig(ctx, live, arc)
		if err != nil {
			t.Fatalf("%s: diffConfig: %v", f.table, err)
		}
		for _, g := range groups {
			if len(g.Added)+len(g.Changed)+len(g.Removed) != 0 {
				t.Fatalf("%s: identical rows reported as %+v", f.table, g)
			}
		}
	}
}

func TestPreviewImport_UnchangedArchiveReportsNoChangesInAnyCategory(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
	for _, f := range tableFixtures() {
		for _, db := range []*sql.DB{live, arc} {
			if f.singleton {
				f.insert(t, db, "", 0)
				continue
			}
			for i, name := range f.names[:2] {
				if f.table != "schedule_jobs" && f.table != "host_config" {
					name = fmt.Sprintf("%s-%d", f.table, i)
				}
				f.insert(t, db, name, 0)
			}
		}
	}
	groups, err := diffConfig(ctx, live, arc)
	if err != nil {
		t.Fatalf("diffConfig: %v", err)
	}
	var categories []string
	for _, g := range groups {
		categories = append(categories, g.Category)
		if len(g.Added)+len(g.Changed)+len(g.Removed) != 0 {
			t.Errorf("%s reports %+v for identical databases", g.Category, g)
		}
	}
	if !slices.Equal(categories, importCategories) {
		t.Fatalf("categories = %v, want every one of %v in order", categories, importCategories)
	}
}

// Columns that record what the running system did since, not what an
// administrator configured, are not a change.
func TestPreviewImport_IgnoresRuntimeColumns(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
	byName := map[string]tableFixture{}
	for _, f := range tableFixtures() {
		byName[f.table] = f
	}
	for _, db := range []*sql.DB{live, arc} {
		byName["shares"].insert(t, db, "media", 0)
		byName["users"].insert(t, db, "alice", 0)
		byName["schedule_jobs"].insert(t, db, "appdata_backup", 0)
		byName["backup_destinations"].insert(t, db, "boot", 0)
		byName["host_config"].insert(t, db, "samba", 0)
		byName["external_disks"].insert(t, db, "usb1", 0)
		byName["array_maintenance"].insert(t, db, "", 0)
		mustExec(t, db, `INSERT OR IGNORE INTO schema_info (id, installation_id, created_at) VALUES (1, 'inst', 't')`)
	}
	mustExec(t, live,
		`UPDATE shares SET updated_at = '2026-09-01T00:00:00Z'`,
		`UPDATE users SET last_login_at = '2026-09-01T00:00:00Z', totp_last_step = 5`,
		`UPDATE schedule_jobs SET last_run_at = '2026-09-01T00:00:00Z'`,
		`UPDATE backup_destinations SET last_successful_backup_at = '2026-09-01T00:00:00Z', stale_alerted_at = '2026-09-02T00:00:00Z'`,
		`UPDATE host_config SET applied_at = '2026-09-01T00:00:00Z'`,
		`UPDATE external_disks SET id = id + 100`,
		`UPDATE array_maintenance SET updated_at = '2026-09-01T00:00:00Z'`,
		`UPDATE schema_info SET created_at = 'later', previous_version = '0.1.0'`,
	)
	groups, err := diffConfig(ctx, live, arc)
	if err != nil {
		t.Fatalf("diffConfig: %v", err)
	}
	for _, g := range groups {
		if len(g.Added)+len(g.Changed)+len(g.Removed) != 0 {
			t.Fatalf("%s reports %+v for runtime-only differences", g.Category, g)
		}
	}
}

// A secret is compared, so a rotated one is reported as a change, and its
// value is never carried out of the database.
func TestPreviewImport_ReportsAChangedSecretWithoutItsValue(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
	acme := fixtureFor(t, "acme_config")
	acme.insert(t, live, "", 0)
	acme.insert(t, arc, "", 0)
	mustExec(t, arc, `UPDATE acme_config SET dns_secret = x'deadbeef'`)

	groups, err := diffConfig(ctx, live, arc)
	if err != nil {
		t.Fatalf("diffConfig: %v", err)
	}
	g := groupByCategory(t, groups, CategorySystem)
	if want := changesOf("acme", ""); !reflect.DeepEqual(g.Changed, want) {
		t.Fatalf("Changed = %v, want %v", g.Changed, want)
	}
}

// Permissions, memberships, tokens and routes are named by the parent rows'
// names when they exist, and by the id otherwise.
func TestPreviewImport_NamesRowsByTheirParents(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
	mustExec(t, arc,
		`INSERT INTO shares (name, cache_mode, create_policy, smb_enabled, smb_guest, smb_read_only, smb_browseable, smb_recycle, smb_time_machine, created_at, updated_at)
			VALUES ('media', 'array-only', 'mfs', 1, 0, 0, 1, 0, 0, 't', 't')`,
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('u-1', 'alice', 'h', 'viewer', 0, 't')`,
		`INSERT INTO user_groups (id, name, created_at) VALUES ('g-1', 'family', 't')`,
		`INSERT INTO notify_channels (id, name, "type", enabled, config, created_at, updated_at) VALUES ('c-1', 'Phone', 'ntfy', 1, '{}', 't', 't')`,
		`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES ('media', 'u-1', 'read-only')`,
		`INSERT INTO share_group_permissions (share_name, group_id, access) VALUES ('media', 'g-1', 'read-only')`,
		`INSERT INTO user_group_members (group_id, user_id) VALUES ('g-1', 'u-1')`,
		`INSERT INTO api_tokens (token_hash, user_id, name, role, created_at) VALUES ('h1', 'u-1', 'script', 'viewer', 't')`,
		`INSERT INTO notify_routes (event_type, channel_id) VALUES ('disk_failed', 'c-1')`,
	)
	groups, err := diffConfig(ctx, live, arc)
	if err != nil {
		t.Fatalf("diffConfig: %v", err)
	}
	var got []string
	for _, g := range groups {
		for _, c := range g.Added {
			got = append(got, c.Kind+"="+c.Name)
		}
	}
	slices.Sort(got)
	want := []string{
		"api_token=alice / script",
		"notification_channel=Phone",
		"notification_route=disk_failed / Phone",
		"share=media",
		"share_group_permission=media / family",
		"share_user_permission=media / alice",
		"user=alice",
		"user_group=family",
		"user_group_member=family / alice",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("added = %v, want %v", got, want)
	}
}

func TestPreviewImport_ListsItemsInNameOrder(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arc := openMigratedDB(t, filepath.Join(t.TempDir(), "arc.db"))
	shares := fixtureFor(t, "shares")
	for _, n := range []string{"zeta", "alpha", "mid"} {
		shares.insert(t, arc, n, 0)
	}
	groups, err := diffConfig(ctx, live, arc)
	if err != nil {
		t.Fatalf("diffConfig: %v", err)
	}
	if got, want := groupByCategory(t, groups, CategoryShares).Added, changesOf("share", "alpha", "mid", "zeta"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Added = %v, want %v", got, want)
	}
}

func stagePreviewArchive(t *testing.T, arc *sql.DB, arcPath string) string {
	t.Helper()
	if _, err := arc.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpointing the archive database: %v", err)
	}
	if err := arc.Close(); err != nil {
		t.Fatalf("closing the archive database: %v", err)
	}
	dir := filepath.Dir(arcPath)
	m := buildManifest("archive-host", "9.9.9", time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC), map[string]string{})
	if err := writeManifest(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	return dir
}

func TestPreviewImport_ReportsTheArchiveAndItsBlockers(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arcPath := filepath.Join(t.TempDir(), "state.db")
	arc := openMigratedDB(t, arcPath)
	fixtureFor(t, "shares").insert(t, arc, "photos", 0)
	mustExec(t, arc, `UPDATE machine_key_check SET check_value = x'bb'`)
	dir := stagePreviewArchive(t, arc, arcPath)

	p, err := PreviewImport(ctx, live, dir)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if p.Host != "archive-host" || p.HoservaVersion != "9.9.9" || !p.Timestamp.Equal(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("archive identity = %s / %s / %s", p.Host, p.HoservaVersion, p.Timestamp)
	}
	if p.ArchiveSchemaVersion == "" || p.ArchiveSchemaVersion != p.LiveSchemaVersion {
		t.Fatalf("schema versions = %q / %q", p.ArchiveSchemaVersion, p.LiveSchemaVersion)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Code != RefusalOtherInstallation || p.Blockers[0].Message != ErrArchiveOtherInstallation.Error() {
		t.Fatalf("blockers = %+v, want the other-installation refusal", p.Blockers)
	}
	if got := groupByCategory(t, p.Groups, CategoryShares).Added; !reflect.DeepEqual(got, changesOf("share", "photos")) {
		t.Fatalf("a blocked archive still lists its changes: shares added = %v", got)
	}
	if len(p.Notes) != 1 || p.Notes[0].Code != NoteSessionsReplaced || !strings.Contains(p.Notes[0].Message, "signed out") {
		t.Fatalf("notes = %+v", p.Notes)
	}
}

func TestPreviewImport_DifferentSchemaVersionIsABlockerWithNoComparison(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arcPath := filepath.Join(t.TempDir(), "state.db")
	arc := openMigratedDB(t, arcPath)
	fixtureFor(t, "shares").insert(t, arc, "photos", 0)
	mustExec(t, arc, `DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`)
	dir := stagePreviewArchive(t, arc, arcPath)

	p, err := PreviewImport(ctx, live, dir)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Code != RefusalIncompatibleArchive {
		t.Fatalf("blockers = %+v, want incompatible_archive", p.Blockers)
	}
	if p.ArchiveSchemaVersion == p.LiveSchemaVersion {
		t.Fatalf("schema versions = %q / %q, want them to differ", p.ArchiveSchemaVersion, p.LiveSchemaVersion)
	}
	if len(p.Groups) != 0 {
		t.Fatalf("groups = %+v, want none when the schemas cannot be compared", p.Groups)
	}
}

func TestPreviewImport_AnArchiveWithoutASchemaVersionIsUnreadable(t *testing.T) {
	ctx := context.Background()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arcPath := filepath.Join(t.TempDir(), "state.db")
	arc, err := sql.Open("sqlite", arcPath)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	mustExec(t, arc, `CREATE TABLE unrelated (x TEXT)`)
	dir := stagePreviewArchive(t, arc, arcPath)

	_, err = PreviewImport(ctx, live, dir)
	if _, ok := err.(*UnreadableArchiveError); !ok {
		t.Fatalf("PreviewImport err = %v (%T), want *UnreadableArchiveError", err, err)
	}
}
