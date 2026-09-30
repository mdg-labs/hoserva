package api

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// importFilesEnv is an import test handler whose config root, templates
// and stacks are real directories and whose pre-import backup goes to a
// real destination, with RegenerateConfig recording what it found.
type importFilesEnv struct {
	h     *Handler
	db    *sql.DB
	root  string
	paths backup.Paths
	dest  string

	regenCalls int
	regenErr   error
	// atRegen is what the database and the directories held when
	// RegenerateConfig ran.
	atRegen struct {
		shares []string
		custom string
	}
	regenHook func()
}

func newImportFilesEnv(t *testing.T) *importFilesEnv {
	t.Helper()
	h, registry, db, dbPath := newImportTestHandlerWithRegistry(t)
	registry.Register(job.TypeSync, true, func(ctx context.Context, rc *job.RunContext) error { return nil })
	root := t.TempDir()
	e := &importFilesEnv{h: h, db: db, root: root, dest: t.TempDir()}
	e.paths = backup.Paths{
		DBPath:       dbPath,
		ConfigRoot:   filepath.Join(root, "etc", "hoserva"),
		TemplatesDir: filepath.Join(root, "state", "templates"),
		StacksDir:    filepath.Join(root, "state", "stacks"),
	}
	for _, d := range []string{"etc", "state"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.Backup.Paths = e.paths
	h.Backup.Destinations = []backup.Destination{{
		ID: "boot", Name: "Boot device", Path: e.dest, Enabled: true,
		Retention: backup.Retention{Daily: backup.DefaultRetentionDaily, Weekly: backup.DefaultRetentionWeekly, Monthly: backup.DefaultRetentionMonthly},
	}}
	h.RegenerateConfig = func(ctx context.Context) error {
		e.regenCalls++
		rows, err := store.NewShareStore(db).List(ctx)
		if err != nil {
			return err
		}
		e.atRegen.shares = nil
		for _, r := range rows {
			e.atRegen.shares = append(e.atRegen.shares, r.Name)
		}
		if b, err := os.ReadFile(filepath.Join(e.paths.ConfigRoot, "smb.custom.conf")); err == nil {
			e.atRegen.custom = string(b)
		}
		if e.regenHook != nil {
			e.regenHook()
		}
		return e.regenErr
	}
	return e
}

func putTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// dirSnapshot records every entry of dir: its kind and content, or a link's
// target.
func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link->" + target
		case fi.IsDir():
			out[rel] = "dir"
		default:
			b, _ := os.ReadFile(p)
			out[rel] = "file " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *importFilesEnv) runtimeSnapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, dir := range map[string]string{"custom": e.paths.ConfigRoot, "templates": e.paths.TemplatesDir, "stacks": e.paths.StacksDir} {
		for k, v := range dirSnapshot(t, dir) {
			out[name+"/"+k] = v
		}
	}
	return out
}

func (e *importFilesEnv) preImportArchives(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(e.dest)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		if strings.Contains(en.Name(), ".pre-import.") {
			names = append(names, en.Name())
		}
	}
	return names
}

var (
	exportedCustom    = map[string]string{"smb.custom.conf": "exported smb", "nested/x.custom.conf": "exported nested", "secret.key": "not restored"}
	exportedTemplates = map[string]string{"app/template.json": "exported template"}
	exportedStacks    = map[string]string{"web/docker-compose.yml": "exported compose", "web/meta.json": "exported meta", "web/.env": "TOKEN=exported"}
)

// seedAndExport seeds the directories and a share, exports, and then
// changes all of it: the share is deleted, custom files edited and removed
// or added, a template edited, a stack's compose file edited, a stack's
// .env changed.
func (e *importFilesEnv) seedAndExport(t *testing.T) []byte {
	t.Helper()
	putTree(t, e.paths.ConfigRoot, exportedCustom)
	putTree(t, e.paths.TemplatesDir, exportedTemplates)
	putTree(t, e.paths.StacksDir, exportedStacks)
	insertSentinelShare(t, e.db, "media")
	archive := exportBytes(t, e.h)

	putTree(t, e.paths.ConfigRoot, map[string]string{"smb.custom.conf": "edited after export", "added.custom.conf": "added after export", "secret.key": "changed, and not a custom file"})
	if err := os.Remove(filepath.Join(e.paths.ConfigRoot, "nested", "x.custom.conf")); err != nil {
		t.Fatal(err)
	}
	putTree(t, e.paths.TemplatesDir, map[string]string{"app/template.json": "edited template", "extra.json": "added template"})
	putTree(t, e.paths.StacksDir, map[string]string{"web/docker-compose.yml": "edited compose", "web/.env": "TOKEN=live", "other/compose.yml": "added stack"})
	if err := store.NewShareStore(e.db).Delete(context.Background(), "media"); err != nil {
		t.Fatal(err)
	}
	return archive
}

func importErr(t *testing.T, err error, status int, code string) *apiError {
	t.Helper()
	ae, ok := err.(*apiError)
	if !ok || ae.statusCode != status || ae.code != code {
		t.Fatalf("err = %v (%T), want (%d, %s)", err, err, status, code)
	}
	return ae
}

func TestImportConfig_RestoresTheFilesThenRegeneratesFromTheRestoredDatabase(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	appdata := filepath.Join(e.root, "appdata")
	putTree(t, appdata, map[string]string{"c/data": "appdata"})
	appdataBefore := dirSnapshot(t, appdata)

	if _, err := e.h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	cfg := dirSnapshot(t, e.paths.ConfigRoot)
	if cfg["smb.custom.conf"] != "file exported smb" || cfg["nested/x.custom.conf"] != "file exported nested" {
		t.Errorf("custom config not restored: %v", cfg)
	}
	if _, ok := cfg["added.custom.conf"]; ok {
		t.Error("a *.custom.conf added after the export was kept")
	}
	if cfg["secret.key"] != "file changed, and not a custom file" {
		t.Errorf("a file that is not *.custom.conf was touched: %q", cfg["secret.key"])
	}
	tpl := dirSnapshot(t, e.paths.TemplatesDir)
	if tpl["app/template.json"] != "file exported template" {
		t.Errorf("template not restored: %v", tpl)
	}
	if _, ok := tpl["extra.json"]; ok {
		t.Error("a template added after the export was kept")
	}
	stk := dirSnapshot(t, e.paths.StacksDir)
	if stk["web/docker-compose.yml"] != "file exported compose" || stk["web/meta.json"] != "file exported meta" {
		t.Errorf("stack not restored: %v", stk)
	}
	if stk["web/.env"] != "file TOKEN=live" {
		t.Errorf("a stack's .env was touched: %q", stk["web/.env"])
	}
	if stk["other/compose.yml"] != "" {
		t.Error("a stack added after the export kept its compose file")
	}
	if !reflect.DeepEqual(appdataBefore, dirSnapshot(t, appdata)) {
		t.Error("appdata changed")
	}

	if e.regenCalls != 1 {
		t.Fatalf("RegenerateConfig ran %d times, want once", e.regenCalls)
	}
	if !slices.Equal(e.atRegen.shares, []string{"media"}) || e.atRegen.custom != "exported smb" {
		t.Fatalf("when RegenerateConfig ran the database held shares %v and smb.custom.conf %q, want the restored ones", e.atRegen.shares, e.atRegen.custom)
	}
	if entries, _ := filepath.Glob(filepath.Join(e.root, "*", ".hoserva-restore-*")); len(entries) != 0 {
		t.Errorf("staging directories left behind: %v", entries)
	}
}

// The regeneration runs while the scheduler's restore hold is still taken,
// so no job can start between the database being replaced and the
// configuration following it (doc 01 §4).
func TestImportConfig_RegeneratesUnderTheRestoreHold(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)

	var submitErr error
	e.regenHook = func() { _, submitErr = e.h.Scheduler.Submit(ctx, job.TypeSync, nil, nil) }
	if _, err := e.h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if !errors.Is(submitErr, job.ErrDatabaseRestoreInProgress) {
		t.Fatalf("Submit during the regeneration = %v, want ErrDatabaseRestoreInProgress", submitErr)
	}
	if _, err := e.h.Scheduler.Submit(ctx, job.TypeSync, nil, nil); err != nil {
		t.Fatalf("Submit after the import = %v, want the hold released", err)
	}
}

// The data-loss scenario for staging: a file of the archive that cannot be
// staged. Here the extracted tree changes between verification and the
// restore, so the staged copy no longer matches the manifest. The database
// and every directory are exactly as they were, and the same archive
// imports once the fault is gone.
func TestImportConfig_AFileThatDoesNotStageLeavesTheDatabaseAndEveryDirectoryUntouched(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	dbBefore := liveFingerprint(t, e.db)
	filesBefore := e.runtimeSnapshot(t)
	rootBefore := dirSnapshot(t, e.root)

	importBeforeStageHookForTest = func(tree string) {
		if err := os.WriteFile(filepath.Join(tree, "templates", "app", "template.json"), []byte("changed after verification"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { importBeforeStageHookForTest = nil })

	_, err := e.h.ImportConfig(ctx, importReq(archive))
	ae := importErr(t, err, 500, "import_failed")
	if !strings.Contains(ae.message, "nothing was changed") {
		t.Errorf("message = %q", ae.message)
	}
	if got := liveFingerprint(t, e.db); got != dbBefore {
		t.Fatalf("the database changed:\nbefore %s\nafter  %s", dbBefore, got)
	}
	if got := e.runtimeSnapshot(t); !reflect.DeepEqual(got, filesBefore) {
		t.Fatalf("a runtime directory changed:\nbefore %v\nafter  %v", filesBefore, got)
	}
	if got := dirSnapshot(t, e.root); !reflect.DeepEqual(got, rootBefore) {
		t.Fatalf("something was left beside the runtime directories:\nbefore %v\nafter  %v", rootBefore, got)
	}
	if e.regenCalls != 0 {
		t.Fatal("RegenerateConfig ran for an import that never started")
	}

	importBeforeStageHookForTest = nil
	if _, err := e.h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := dirSnapshot(t, e.paths.TemplatesDir)["app/template.json"]; got != "file exported template" {
		t.Fatalf("template after the retry = %q", got)
	}
}

// The data-loss scenario for applying: after the database was restored, the
// stacks cannot be put in place (a compose file has become a directory).
// Custom config and templates are wholly the archive's, stacks wholly as
// they were, the database is restored, the configuration is still
// regenerated from it, and the error names all of it and the archive to go
// back to.
func TestImportConfig_AFailureAfterTheDatabaseRestoreNamesEachCategoryAndThePreImportArchive(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)

	var stacksAtFailure map[string]string
	importPostRestoreHookForTest = func() {
		compose := filepath.Join(e.paths.StacksDir, "web", "docker-compose.yml")
		if err := os.Remove(compose); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(compose, 0o755); err != nil {
			t.Fatal(err)
		}
		stacksAtFailure = dirSnapshot(t, e.paths.StacksDir)
	}
	t.Cleanup(func() { importPostRestoreHookForTest = nil })

	_, err := e.h.ImportConfig(ctx, importReq(archive))
	ae := importErr(t, err, 500, "import_failed")
	archives := e.preImportArchives(t)
	if len(archives) != 1 {
		t.Fatalf("pre-import archives = %v", archives)
	}
	for _, want := range []string{
		"the database was restored",
		"restored: custom config files, app templates",
		"left as they were: app stacks",
		archives[0],
		"Boot device",
	} {
		if !strings.Contains(ae.message, want) {
			t.Errorf("message %q does not contain %q", ae.message, want)
		}
	}

	if got, _ := store.NewShareStore(e.db).List(ctx); len(got) != 1 || got[0].Name != "media" {
		t.Errorf("shares = %+v, want the restored database", got)
	}
	cfg := dirSnapshot(t, e.paths.ConfigRoot)
	if cfg["smb.custom.conf"] != "file exported smb" || cfg["nested/x.custom.conf"] != "file exported nested" || cfg["added.custom.conf"] != "" {
		t.Errorf("custom config is neither wholly old nor wholly new: %v", cfg)
	}
	tpl := dirSnapshot(t, e.paths.TemplatesDir)
	if tpl["app/template.json"] != "file exported template" || tpl["extra.json"] != "" {
		t.Errorf("templates are neither wholly old nor wholly new: %v", tpl)
	}
	if got := dirSnapshot(t, e.paths.StacksDir); !reflect.DeepEqual(got, stacksAtFailure) {
		t.Errorf("stacks changed although their apply failed:\nbefore %v\nafter  %v", stacksAtFailure, got)
	}
	if e.regenCalls != 1 {
		t.Errorf("RegenerateConfig ran %d times after the database was replaced, want once", e.regenCalls)
	}
}

func TestImportConfig_AFailedRegenerationIsReportedWithThePreImportArchive(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	e.regenErr = errors.New("smb.conf is not writable")

	_, err := e.h.ImportConfig(ctx, importReq(archive))
	ae := importErr(t, err, 500, "import_failed")
	archives := e.preImportArchives(t)
	if len(archives) != 1 {
		t.Fatalf("pre-import archives = %v", archives)
	}
	for _, want := range []string{"regenerating the configuration", "smb.conf is not writable", archives[0]} {
		if !strings.Contains(ae.message, want) {
			t.Errorf("message %q does not contain %q", ae.message, want)
		}
	}
	if got := dirSnapshot(t, e.paths.ConfigRoot)["smb.custom.conf"]; got != "file exported smb" {
		t.Errorf("the files were not restored: %q", got)
	}
}

// The data-loss scenario for links: a *.custom.conf in the config root that
// is a symbolic link to a file elsewhere. The import is refused before the
// pre-import backup takes its slot, and the file it points at is unchanged.
func TestImportConfig_RefusesToWriteThroughASymbolicLink(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.paths.ConfigRoot, "smb.custom.conf")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dbBefore := liveFingerprint(t, e.db)
	filesBefore := e.runtimeSnapshot(t)

	_, err := e.h.ImportConfig(ctx, importReq(archive))
	ae := importErr(t, err, 409, "restore_path_unsafe")
	if !strings.Contains(ae.message, "smb.custom.conf") {
		t.Errorf("message = %q, want it to name the link", ae.message)
	}
	if b, _ := os.ReadFile(outside); string(b) != "outside" {
		t.Fatalf("the file the link points at was written: %q", b)
	}
	if got := liveFingerprint(t, e.db); got != dbBefore {
		t.Fatal("the database changed")
	}
	if got := e.runtimeSnapshot(t); !reflect.DeepEqual(got, filesBefore) {
		t.Fatal("a runtime directory changed")
	}
	if n := e.preImportArchives(t); len(n) != 0 {
		t.Fatalf("a refused import took a pre-import backup: %v", n)
	}
	if e.regenCalls != 0 {
		t.Fatal("RegenerateConfig ran for a refused import")
	}
}

func TestImportConfig_WithoutARegenerationHookIsNotConfigured(t *testing.T) {
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	e.h.RegenerateConfig = nil
	dbBefore := liveFingerprint(t, e.db)

	_, err := e.h.ImportConfig(context.Background(), importReq(archive))
	_ = importErr(t, err, 501, "not_configured")
	if got := liveFingerprint(t, e.db); got != dbBefore {
		t.Fatal("the database changed")
	}
}

// TestImportConfig_ReplacesRunningDatabaseWithoutCorruption's scenario
// through the whole apply: files in every directory, an interim mutation,
// the import, a write on the same pool and a reopen.
func TestImportConfig_FullApplyReplacesRunningDatabaseWithoutCorruption(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('interim', 'interim-admin', 'x', 'admin', 0, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	if _, err := e.h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('post', 'post-import', 'x', 'viewer', 0, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("write on the live pool right after import: %v", err)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := sql.Open("sqlite", store.DSN(e.paths.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	var check string
	if err := fresh.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q (%v)", check, err)
	}
	var interim, post int
	_ = fresh.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = 'interim'").Scan(&interim)
	_ = fresh.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = 'post'").Scan(&post)
	if interim != 0 || post != 1 {
		t.Fatalf("interim = %d, post = %d, want the exported state plus the post-import write", interim, post)
	}
	list, err := store.NewShareStore(fresh).List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "media" {
		t.Fatalf("shares after the reopen = %+v (%v)", list, err)
	}
	if got := dirSnapshot(t, e.paths.ConfigRoot)["smb.custom.conf"]; got != "file exported smb" {
		t.Fatalf("smb.custom.conf = %q", got)
	}
}

func TestPreviewConfigImport_ListsTheFilesAnImportWouldReplaceAddAndRemove(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	before := e.runtimeSnapshot(t)

	p, err := e.h.PreviewConfigImport(ctx, previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	names := func(cs []apiv1.ConfigImportChange) []string {
		out := []string{}
		for _, c := range cs {
			out = append(out, string(c.Kind)+":"+c.Name)
		}
		return out
	}
	custom := previewGroup(t, p, apiv1.ConfigImportGroupCategoryCustomConfig)
	if !slices.Equal(names(custom.Changed), []string{"custom_config_file:smb.custom.conf"}) ||
		!slices.Equal(names(custom.Added), []string{"custom_config_file:nested/x.custom.conf"}) ||
		!slices.Equal(names(custom.Removed), []string{"custom_config_file:added.custom.conf"}) {
		t.Errorf("custom config group = %+v", custom)
	}
	tpl := previewGroup(t, p, apiv1.ConfigImportGroupCategoryTemplates)
	if !slices.Equal(names(tpl.Changed), []string{"template_file:app/template.json"}) || !slices.Equal(names(tpl.Removed), []string{"template_file:extra.json"}) {
		t.Errorf("templates group = %+v", tpl)
	}
	stk := previewGroup(t, p, apiv1.ConfigImportGroupCategoryStacks)
	if !slices.Equal(names(stk.Changed), []string{"stack_file:web/docker-compose.yml"}) || !slices.Equal(names(stk.Removed), []string{"stack_file:other/compose.yml"}) {
		t.Errorf("stacks group = %+v", stk)
	}
	if got := e.runtimeSnapshot(t); !reflect.DeepEqual(got, before) {
		t.Fatal("the preview changed a runtime directory")
	}
	if entries, _ := filepath.Glob(filepath.Join(e.root, "*", ".hoserva-restore-*")); len(entries) != 0 {
		t.Fatalf("the preview left staging directories: %v", entries)
	}
}

func TestPreviewConfigImport_ALinkTheImportWouldRefuseIsABlocker(t *testing.T) {
	e := newImportFilesEnv(t)
	archive := e.seedAndExport(t)
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.paths.ConfigRoot, "linked.custom.conf")); err != nil {
		t.Fatal(err)
	}

	p, err := e.h.PreviewConfigImport(context.Background(), previewReq(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeRestorePathUnsafe {
		t.Fatalf("blockers = %+v", p.Blockers)
	}
	_, err = e.h.ImportConfig(context.Background(), importReq(archive))
	_ = importErr(t, err, 409, "restore_path_unsafe")
}

// Every category and every refusal the comparison can report is one the API
// schema names, and the schema names no others.
func TestPreviewConfigImport_CategoriesAndBlockersMatchTheSchema(t *testing.T) {
	var categories []string
	for _, c := range (apiv1.ConfigImportGroupCategory("")).AllValues() {
		categories = append(categories, string(c))
	}
	got := backup.ImportCategories()
	slices.Sort(categories)
	slices.Sort(got)
	if !slices.Equal(got, categories) {
		t.Fatalf("categories the comparison reports = %v, schema = %v", got, categories)
	}

	var blockers []string
	for _, c := range (apiv1.ConfigImportBlockerCode("")).AllValues() {
		blockers = append(blockers, string(c))
	}
	want := []string{backup.RefusalIncompatibleArchive, backup.RefusalNewerArchive, backup.RefusalOtherInstallation, backup.RefusalArrayMismatch, backup.RefusalUnsafeRestorePath}
	slices.Sort(blockers)
	slices.Sort(want)
	if !slices.Equal(want, blockers) {
		t.Fatalf("refusals = %v, schema = %v", want, blockers)
	}
}
