package backup

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The categories a preview groups its changes into, in the order it lists
// them.
const (
	CategoryShares        = "shares"
	CategoryAccounts      = "accounts"
	CategorySchedules     = "schedules"
	CategoryNotifications = "notifications"
	CategoryBackup        = "backup"
	CategorySystem        = "system"
)

// NoteSessionsReplaced is the code of a note every preview carries: the
// restored database brings the archive's sessions table with it. The other,
// NoteArrayStateKept, is in arraystate.go.
const NoteSessionsReplaced = "sessions_replaced"

// ImportChange is one item an in-place import would add, change or remove.
// Kind says what sort of thing it is; Name is what a user calls it.
type ImportChange struct {
	Kind string
	Name string
}

// ImportGroup is a category's changes: Added is in the archive and not the
// live database, Removed the reverse, Changed in both with different
// content.
type ImportGroup struct {
	Category string
	Added    []ImportChange
	Changed  []ImportChange
	Removed  []ImportChange
}

// ImportNote is a fact about an import that holds whatever the archive
// contains.
type ImportNote struct {
	Code    string
	Message string
}

// ImportPreview is what an in-place import of an archive would do.
type ImportPreview struct {
	Timestamp            time.Time
	Host                 string
	HoservaVersion       string
	ArchiveSchemaVersion string
	LiveSchemaVersion    string
	Blockers             []ImportRefusal
	// Groups is empty when the schema versions differ: the two databases
	// cannot be compared row by row.
	Groups []ImportGroup
	Notes  []ImportNote
}

// configTable is one table an import replaces and a preview compares. Rows
// are matched on key and shown as label, an SQL expression over the table
// aliased t; every other column, less ignore, decides whether a matched row
// changed. When only is set, just those columns decide it: one table can then
// be several kinds, each reported on its own.
type configTable struct {
	category string
	kind     string
	table    string
	key      []string
	label    string
	ignore   []string
	only     []string
}

const (
	userNameOf  = `COALESCE((SELECT username FROM users u WHERE u.id = t.user_id), t.user_id)`
	groupNameOf = `COALESCE((SELECT name FROM user_groups g WHERE g.id = t.group_id), t.group_id)`
)

// configTables are the tables a preview compares. Every other table an
// import replaces is in uncomparedTables, with the reason it is not listed.
var configTables = []configTable{
	{category: CategoryShares, kind: "share", table: "shares", key: []string{"name"}, label: `t.name`, ignore: []string{"updated_at"}},
	{category: CategoryShares, kind: "share_user_permission", table: "share_user_permissions", key: []string{"share_name", "user_id"}, label: `t.share_name || ' / ' || ` + userNameOf},
	{category: CategoryShares, kind: "share_group_permission", table: "share_group_permissions", key: []string{"share_name", "group_id"}, label: `t.share_name || ' / ' || ` + groupNameOf},

	{category: CategoryAccounts, kind: "user", table: "users", key: []string{"id"}, label: `t.username`, ignore: []string{"last_login_at", "totp_last_step"}},
	{category: CategoryAccounts, kind: "user_group", table: "user_groups", key: []string{"id"}, label: `t.name`},
	{category: CategoryAccounts, kind: "user_group_member", table: "user_group_members", key: []string{"group_id", "user_id"}, label: groupNameOf + ` || ' / ' || ` + userNameOf},
	{category: CategoryAccounts, kind: "api_token", table: "api_tokens", key: []string{"token_hash"}, label: userNameOf + ` || ' / ' || t.name`},

	{category: CategorySchedules, kind: "schedule_chain", table: "schedule_chain", key: []string{"id"}, label: `''`, ignore: []string{"last_run_at", "updated_at"}},
	{category: CategorySchedules, kind: "schedule_job", table: "schedule_jobs", key: []string{"job_id"}, label: `t.job_id`, ignore: []string{"last_run_at", "updated_at"}},

	{category: CategoryNotifications, kind: "notification_channel", table: "notify_channels", key: []string{"id"}, label: `t.name`, ignore: []string{"updated_at"}},
	{category: CategoryNotifications, kind: "notification_route", table: "notify_routes", key: []string{"event_type", "channel_id"}, label: `t.event_type || ' / ' || COALESCE((SELECT name FROM notify_channels c WHERE c.id = t.channel_id), t.channel_id)`},
	{category: CategoryNotifications, kind: "notification_severity", table: "notify_event_severity", key: []string{"event_type"}, label: `t.event_type`},
	{category: CategoryNotifications, kind: "notification_quiet_hours", table: "notify_quiet_hours", key: []string{"id"}, label: `''`, ignore: []string{"updated_at"}},

	{category: CategoryBackup, kind: "backup_destination", table: "backup_destinations", key: []string{"id"}, label: `t.name`, ignore: []string{"last_successful_backup_at", "stale_alerted_at"}},
	{category: CategoryBackup, kind: "appdata_backup_container", table: "appdata_backup_containers", key: []string{"container"}, label: `t.container`, ignore: []string{"updated_at"}},
	{category: CategoryBackup, kind: "backup_recipient", table: "backup_recipient", key: []string{"id"}, label: `''`},

	{category: CategorySystem, kind: "acme", table: "acme_config", key: []string{"id"}, label: `''`, ignore: []string{"last_error", "updated_at"}},
	{category: CategorySystem, kind: "ups", table: "ups_config", key: []string{"id"}, label: `''`, ignore: []string{"updated_at"}},
	{category: CategorySystem, kind: "array_settings", table: "array_settings", key: []string{"id"}, label: `''`},
	{category: CategorySystem, kind: "host_config", table: "host_config", key: []string{"kind"}, label: `t.kind`, ignore: []string{"applied_at"}},
	{category: CategorySystem, kind: "external_disk", table: "external_disks", key: []string{"label"}, label: `t.label`, ignore: []string{"id"}},
	{category: CategorySystem, kind: "hostname", table: "schema_info", key: []string{"id"}, label: `''`, only: []string{"hostname"}},
	{category: CategorySystem, kind: "timezone", table: "schema_info", key: []string{"id"}, label: `''`, only: []string{"timezone"}},
	{category: CategorySystem, kind: "backup_passphrase", table: "schema_info", key: []string{"id"}, label: `''`, only: []string{"backup_passphrase"}},
	{category: CategorySystem, kind: "update_channel", table: "schema_info", key: []string{"id"}, label: `''`, only: []string{"update_channel"}},
	{category: CategorySystem, kind: "update_check", table: "schema_info", key: []string{"id"}, label: `''`, only: []string{"update_check_enabled"}},
}

// uncomparedTables are the tables an import replaces that a preview does not
// list, each with why. Every table in the schema is here or in configTables.
var uncomparedTables = map[string]string{
	"jobs":                       "history: the record of past and queued jobs",
	"audit_log":                  "history: the record of past actions",
	"spin_events":                "history: the record of past disk spin-ups and spin-downs",
	"notify_deliveries":          "history: the record of past notification deliveries",
	"notify_alerts":              "runtime: the state of raised alerts",
	"share_usage":                "runtime: what a past sync measured",
	"share_usage_computed_at":    "runtime: when share_usage was measured",
	"mover_run_result":           "runtime: the last mover run's result",
	"cache_usage_breakdown":      "runtime: what the last mover run measured",
	"restore_drill_result":       "runtime: the last restore drill's result",
	"backup_destination_enabled": "runtime: when a destination was last switched on, which its staleness counts from",
	"sessions":                   "replaced and reported by NoteSessionsReplaced",
	"array_maintenance":          "kept: the import writes the live row into the restore (KeepArrayState) and reports NoteArrayStateKept",
	"array_disks":                "refused by CheckImport when it differs: an archive of another array is never imported",
	"relocation_manifest":        "refused by CheckImport when it differs: an archive of another array is never imported",
	"relocation_removing_disks":  "refused by CheckImport when it differs: an archive of another array is never imported",
	"stacks":                     "compared as the stack_file changes: the rows hold what each stack's compose file, .env and meta.json are generated from",
	"machine_key_check":          "refused by CheckImport when it differs: it identifies the installation",
	"schema_migrations":          "the schema version, which CheckImport requires to match",
}

// uncomparedColumns are the schema_info columns no configTables entry
// compares, each with why; table.column is the key.
var uncomparedColumns = map[string]string{
	"schema_info.id":               "the singleton row's key",
	"schema_info.installation_id":  "identifies the installation, which CheckImport requires to match",
	"schema_info.created_at":       "runtime: when the installation was first started",
	"schema_info.previous_version": "runtime: the version before the last upgrade",
}

var importCategories = []string{CategoryShares, CategoryAccounts, CategorySchedules, CategoryNotifications, CategoryBackup, CategorySystem}

// fileGroupKinds is the kind a preview reports for each category of files
// an import restores besides the database (RestoreFiles). Their categories
// are FilesCustomConfig, FilesTemplates and FilesStacks, listed after the
// database's.
var fileGroupKinds = map[string]string{
	FilesCustomConfig: "custom_config_file",
	FilesTemplates:    "template_file",
	FilesStacks:       "stack_file",
}

// ImportChangeKinds lists every kind a preview can report.
func ImportChangeKinds() []string {
	kinds := make([]string, 0, len(configTables)+len(fileGroupKinds))
	for _, t := range configTables {
		kinds = append(kinds, t.kind)
	}
	for _, c := range filesCategories {
		kinds = append(kinds, fileGroupKinds[c])
	}
	return kinds
}

// ImportCategories lists every category a preview can report, in the order
// it lists them.
func ImportCategories() []string {
	return slices.Concat(importCategories, filesCategories)
}

// PreviewImport reports what restoring the archive unpacked in stagingDir
// (its manifest.json, state.db and files) over live and the directories in
// paths would do, and writes nothing: both databases are only read, the
// archive's opened read-only. A refusal CheckImport finds, or a path
// RestoreFiles would refuse, is a blocker; the comparison still runs unless
// the schema versions differ. It reads the two databases and the files
// RestoreFiles manages only (Q13).
func PreviewImport(ctx context.Context, live *sql.DB, paths Paths, stagingDir string, opts ...FilesOption) (ImportPreview, error) {
	manifest, err := readManifest(filepath.Join(stagingDir, "manifest.json"))
	if err != nil {
		return ImportPreview{}, fmt.Errorf("reading the archive's manifest: %w", err)
	}
	arc, err := openArchiveDB(filepath.Join(stagingDir, "state.db"))
	if err != nil {
		return ImportPreview{}, err
	}
	defer func() { _ = arc.Close() }()

	check, err := checkImport(ctx, live, arc)
	if err != nil {
		return ImportPreview{}, err
	}
	p := newImportPreview(manifest, check.ArchiveSchemaVersion, check.LiveSchemaVersion)
	if check.Refusal != nil {
		p.Blockers = append(p.Blockers, *check.Refusal)
	}
	files, err := p.planFiles(stagingDir, paths, opts...)
	if err != nil {
		return ImportPreview{}, err
	}
	if check.ArchiveSchemaVersion != check.LiveSchemaVersion {
		return p, nil
	}
	if err := p.compare(ctx, live, arc, files); err != nil {
		return ImportPreview{}, err
	}
	return p, nil
}

func newImportPreview(manifest Manifest, archiveSchema, liveSchema string) ImportPreview {
	return ImportPreview{
		Timestamp:            manifest.Timestamp,
		Host:                 manifest.Host,
		HoservaVersion:       manifest.Hoserva,
		ArchiveSchemaVersion: archiveSchema,
		LiveSchemaVersion:    liveSchema,
		Blockers:             []ImportRefusal{},
		Groups:               []ImportGroup{},
		Notes: []ImportNote{
			{
				Code:    NoteSessionsReplaced,
				Message: "Active sign-in sessions are replaced by the archive's, so the current user is signed out.",
			},
			{
				Code:    NoteArrayStateKept,
				Message: "The array's current state, running, in maintenance mode or stopped, is kept: the import does not restore the archive's.",
			},
		},
	}
}

// planFiles is PlanFiles for a preview: a path the restore would refuse is
// a blocker, and the files' categories are then listed without changes.
func (p *ImportPreview) planFiles(stagingDir string, paths Paths, opts ...FilesOption) ([]FileChanges, error) {
	files, err := PlanFiles(stagingDir, paths, opts...)
	var unsafe *UnsafeRestorePathError
	switch {
	case errors.As(err, &unsafe):
		p.Blockers = append(p.Blockers, ImportRefusal{Code: RefusalUnsafeRestorePath, Message: err.Error()})
		files = nil
		for _, c := range filesCategories {
			files = append(files, FileChanges{Category: c})
		}
	case err != nil:
		return nil, fmt.Errorf("comparing the archive's files: %w", err)
	}
	return files, nil
}

// compare fills p's groups from the two databases, then the files'.
func (p *ImportPreview) compare(ctx context.Context, live, arc *sql.DB, files []FileChanges) error {
	var err error
	p.Groups, err = diffConfig(ctx, live, arc)
	if err != nil {
		return err
	}
	for _, c := range files {
		p.Groups = append(p.Groups, ImportGroup{
			Category: c.Category,
			Added:    fileChanges(fileGroupKinds[c.Category], c.Added),
			Changed:  fileChanges(fileGroupKinds[c.Category], c.Replaced),
			Removed:  fileChanges(fileGroupKinds[c.Category], c.Removed),
		})
	}
	return nil
}

func fileChanges(kind string, names []string) []ImportChange {
	out := make([]ImportChange, len(names))
	for i, n := range names {
		out[i] = ImportChange{Kind: kind, Name: n}
	}
	return out
}

// DiffImport compares the database at archiveDB, the staged state.db of a
// verified archive whose schema version matches live's, with live, in
// ImportCategories' database categories and PreviewImport's terms. Both
// databases are only read.
func DiffImport(ctx context.Context, live *sql.DB, archiveDB string) ([]ImportGroup, error) {
	arc, err := openArchiveDB(archiveDB)
	if err != nil {
		return nil, err
	}
	defer func() { _ = arc.Close() }()
	return diffConfig(ctx, live, arc)
}

func diffConfig(ctx context.Context, live, arc *sql.DB) ([]ImportGroup, error) {
	groups := make([]ImportGroup, len(importCategories))
	for i, c := range importCategories {
		groups[i] = ImportGroup{Category: c, Added: []ImportChange{}, Changed: []ImportChange{}, Removed: []ImportChange{}}
	}
	for _, t := range configTables {
		liveRows, err := t.load(ctx, live)
		if err != nil {
			return nil, fmt.Errorf("reading the live %s: %w", t.table, err)
		}
		arcRows, err := t.load(ctx, arc)
		if err != nil {
			return nil, fmt.Errorf("reading the archive's %s: %w", t.table, err)
		}
		g := &groups[slices.Index(importCategories, t.category)]
		g.Added = append(g.Added, t.pick(arcRows, func(k string, r configRow) bool { _, in := liveRows[k]; return !in })...)
		g.Removed = append(g.Removed, t.pick(liveRows, func(k string, r configRow) bool { _, in := arcRows[k]; return !in })...)
		g.Changed = append(g.Changed, t.pick(arcRows, func(k string, r configRow) bool {
			l, in := liveRows[k]
			return in && l.sum != r.sum
		})...)
	}
	return groups, nil
}

type configRow struct {
	label string
	sum   [sha256.Size]byte
}

func (t configTable) pick(rows map[string]configRow, keep func(key string, r configRow) bool) []ImportChange {
	type keyed struct {
		key string
		row configRow
	}
	var picked []keyed
	for k, r := range rows {
		if keep(k, r) {
			picked = append(picked, keyed{k, r})
		}
	}
	slices.SortFunc(picked, func(a, b keyed) int {
		return cmp.Or(strings.Compare(a.row.label, b.row.label), strings.Compare(a.key, b.key))
	})
	out := make([]ImportChange, len(picked))
	for i, p := range picked {
		out[i] = ImportChange{Kind: t.kind, Name: p.row.label}
	}
	return out
}

// load reads every row of the table keyed by its primary key, with the
// label to show for it and a digest of the columns that decide whether it
// changed, so no row's content is kept.
func (t configTable) load(ctx context.Context, db *sql.DB) (map[string]configRow, error) {
	keyParts := make([]string, len(t.key))
	for i, k := range t.key {
		keyParts[i] = "CAST(t." + k + " AS TEXT)"
	}
	query := fmt.Sprintf(`SELECT %s AS row_key, %s AS row_label, t.* FROM %s t`, strings.Join(keyParts, ` || char(31) || `), t.label, t.table)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	compared := make([]bool, len(cols))
	for i := 2; i < len(cols); i++ {
		if t.only != nil {
			compared[i] = slices.Contains(t.only, cols[i])
		} else {
			compared[i] = !slices.Contains(t.ignore, cols[i])
		}
	}
	out := map[string]configRow{}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		h := sha256.New()
		for i := 2; i < len(cols); i++ {
			if !compared[i] {
				continue
			}
			var b []byte
			switch v := vals[i].(type) {
			case []byte:
				b = v
			default:
				b = []byte(fmt.Sprint(v))
			}
			_, _ = fmt.Fprintf(h, "%s:%T:%d:", cols[i], vals[i], len(b))
			_, _ = h.Write(b)
		}
		var r configRow
		r.label = asString(vals[1])
		copy(r.sum[:], h.Sum(nil))
		out[asString(vals[0])] = r
	}
	return out, rows.Err()
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return fmt.Sprint(v)
}
