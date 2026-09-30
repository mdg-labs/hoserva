package api

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
)

const importTestPassphrase = "import-test-passphrase"

// exportWithSecrets is seedAndExport with a backup passphrase configured at
// export time, so the archive carries a secrets.age holding web's .env, and
// then the configuration this handler would try for it set to configured.
func (e *importFilesEnv) exportWithSecrets(t *testing.T, configured backup.SecretSource) []byte {
	t.Helper()
	e.h.Backup.Cipher = backup.FakeSecretCipher{}
	e.h.Backup.Secrets = &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true}
	archive := e.seedAndExport(t)
	e.h.Backup.Secrets = configured
	return archive
}

func withPassphrase(req *apiv1.ImportConfigReq, passphrase string) *apiv1.ImportConfigReq {
	req.Passphrase = apiv1.NewOptString(passphrase)
	return req
}

func restoredCounts(r *apiv1.ConfigImportReport) map[apiv1.ConfigImportRestoredCategory][3]int64 {
	out := map[apiv1.ConfigImportRestoredCategory][3]int64{}
	for _, c := range r.Restored {
		out[c.Category] = [3]int64{c.Added, c.Changed, c.Removed}
	}
	return out
}

func notRestoredOf(r *apiv1.ConfigImportReport) map[string]apiv1.ConfigImportNotRestoredReason {
	out := map[string]apiv1.ConfigImportNotRestoredReason{}
	for _, n := range r.NotRestored {
		out[n.Name] = n.Reason
	}
	return out
}

func envPerm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestImportConfig_ARestoreWithTheConfiguredPassphraseRestoresStackEnvsAndReportsIt(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})

	report, err := e.h.ImportConfig(ctx, importReq(archive))
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	env := filepath.Join(e.paths.StacksDir, "web", ".env")
	if got := dirSnapshot(t, e.paths.StacksDir)["web/.env"]; got != "file TOKEN=exported" {
		t.Errorf("web/.env = %q, want the archive's", got)
	}
	if p := envPerm(t, env); p != 0o600 {
		t.Errorf("web/.env mode = %o, want 600", p)
	}
	if report.Secrets != apiv1.ConfigImportSecretsStatusOpened || len(report.NotRestored) != 0 {
		t.Errorf("report secrets = %s, notRestored = %+v", report.Secrets, report.NotRestored)
	}
	archives := e.preImportArchives(t)
	if len(archives) != 1 || report.PreImportArchive != archives[0] {
		t.Errorf("preImportArchive = %q, archives on the destination = %v", report.PreImportArchive, archives)
	}

	var categories []apiv1.ConfigImportRestoredCategory
	for _, c := range report.Restored {
		categories = append(categories, c.Category)
	}
	wantCategories := []apiv1.ConfigImportRestoredCategory{
		apiv1.ConfigImportRestoredCategoryShares, apiv1.ConfigImportRestoredCategoryAccounts, apiv1.ConfigImportRestoredCategorySchedules,
		apiv1.ConfigImportRestoredCategoryNotifications, apiv1.ConfigImportRestoredCategoryBackup, apiv1.ConfigImportRestoredCategorySystem,
		apiv1.ConfigImportRestoredCategoryCustomConfig, apiv1.ConfigImportRestoredCategoryTemplates, apiv1.ConfigImportRestoredCategoryStacks,
		apiv1.ConfigImportRestoredCategoryStackEnv,
	}
	if !reflect.DeepEqual(categories, wantCategories) {
		t.Fatalf("categories = %v, want %v", categories, wantCategories)
	}
	counts := restoredCounts(report)
	for category, want := range map[apiv1.ConfigImportRestoredCategory][3]int64{
		apiv1.ConfigImportRestoredCategoryShares:       {1, 0, 0},
		apiv1.ConfigImportRestoredCategoryCustomConfig: {1, 1, 1},
		apiv1.ConfigImportRestoredCategoryTemplates:    {0, 1, 1},
		apiv1.ConfigImportRestoredCategoryStacks:       {0, 1, 1},
		apiv1.ConfigImportRestoredCategoryStackEnv:     {0, 1, 0},
	} {
		if counts[category] != want {
			t.Errorf("%s = %v (added, changed, removed), want %v", category, counts[category], want)
		}
	}
}

func TestImportConfig_AnExplicitPassphraseRestoresStackEnvsWithNoneConfigured(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{})

	report, err := e.h.ImportConfig(ctx, withPassphrase(importReq(archive), importTestPassphrase))
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if got := dirSnapshot(t, e.paths.StacksDir)["web/.env"]; got != "file TOKEN=exported" {
		t.Errorf("web/.env = %q, want the archive's", got)
	}
	if report.Secrets != apiv1.ConfigImportSecretsStatusOpened || len(report.NotRestored) != 0 {
		t.Errorf("report secrets = %s, notRestored = %+v", report.Secrets, report.NotRestored)
	}
}

func TestImportConfig_WithoutAPassphraseEverythingButStackEnvsIsRestoredAndTheReportSaysSo(t *testing.T) {
	for name, configured := range map[string]backup.SecretSource{
		"none configured":             &backup.FakeSecretSource{},
		"only a wrong one configured": &backup.FakeSecretSource{Passphrase: "not the passphrase", HasPass: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := newImportFilesEnv(t)
			archive := e.exportWithSecrets(t, configured)

			report, err := e.h.ImportConfig(ctx, importReq(archive))
			if err != nil {
				t.Fatalf("ImportConfig: %v", err)
			}

			stk := dirSnapshot(t, e.paths.StacksDir)
			if stk["web/.env"] != "file TOKEN=live" {
				t.Errorf("web/.env = %q, want the live file kept", stk["web/.env"])
			}
			if stk["web/docker-compose.yml"] != "file exported compose" {
				t.Errorf("web/docker-compose.yml = %q, want the archive's", stk["web/docker-compose.yml"])
			}
			if got := dirSnapshot(t, e.paths.ConfigRoot)["smb.custom.conf"]; got != "file exported smb" {
				t.Errorf("custom config = %q, want the archive's", got)
			}

			wantStatus, wantReason := apiv1.ConfigImportSecretsStatusNoPassphrase, apiv1.ConfigImportNotRestoredReasonNoPassphrase
			if name != "none configured" {
				wantStatus, wantReason = apiv1.ConfigImportSecretsStatusPassphraseIncorrect, apiv1.ConfigImportNotRestoredReasonPassphraseIncorrect
			}
			if report.Secrets != wantStatus {
				t.Errorf("report secrets = %s, want %s", report.Secrets, wantStatus)
			}
			if got := notRestoredOf(report); !reflect.DeepEqual(got, map[string]apiv1.ConfigImportNotRestoredReason{"web": wantReason}) {
				t.Errorf("notRestored = %+v", report.NotRestored)
			}
			for _, n := range report.NotRestored {
				if n.Kind != apiv1.ConfigImportNotRestoredKindStackEnv || n.Message == "" {
					t.Errorf("item %+v has no kind or message", n)
				}
			}
			if c := restoredCounts(report)[apiv1.ConfigImportRestoredCategoryStackEnv]; c != [3]int64{} {
				t.Errorf("stack_env = %v, want nothing restored", c)
			}
			if report.PreImportArchive == "" {
				t.Error("the report names no pre-import archive")
			}
		})
	}
}

// A stack the archive does not hold keeps its .env, and the report lists it
// as left in place.
func TestImportConfig_AStackTheArchiveLacksKeepsItsEnvAndIsReported(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})
	putTree(t, e.paths.StacksDir, map[string]string{"other/.env": "OTHER=live"})

	report, err := e.h.ImportConfig(ctx, importReq(archive))
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if got := dirSnapshot(t, e.paths.StacksDir)["other/.env"]; got != "file OTHER=live" {
		t.Errorf("other/.env = %q, want it kept", got)
	}
	want := map[string]apiv1.ConfigImportNotRestoredReason{"other": apiv1.ConfigImportNotRestoredReasonLeftInPlace}
	if got := notRestoredOf(report); !reflect.DeepEqual(got, want) {
		t.Errorf("notRestored = %+v, want %v", report.NotRestored, want)
	}
}

// The data-loss scenarios for a wrong passphrase: one given explicitly is
// refused before anything is written, even when the configured one would
// have opened the archive.
func TestImportConfig_AnExplicitPassphraseThatDoesNotOpenTheArchiveIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})
	dbBefore := liveFingerprint(t, e.db)
	filesBefore := e.runtimeSnapshot(t)
	rootBefore := dirSnapshot(t, e.root)

	_, err := e.h.ImportConfig(ctx, withPassphrase(importReq(archive), "wrong passphrase"))
	_ = importErr(t, err, 400, "backup_passphrase_incorrect")

	if got := liveFingerprint(t, e.db); got != dbBefore {
		t.Fatal("the database changed")
	}
	if got := e.runtimeSnapshot(t); !reflect.DeepEqual(got, filesBefore) {
		t.Fatalf("a runtime directory changed:\nbefore %v\nafter  %v", filesBefore, got)
	}
	if got := dirSnapshot(t, e.root); !reflect.DeepEqual(got, rootBefore) {
		t.Fatal("something was left beside the runtime directories")
	}
	if n := e.preImportArchives(t); len(n) != 0 {
		t.Fatalf("a refused import took a pre-import backup: %v", n)
	}
	if e.regenCalls != 0 {
		t.Fatal("RegenerateConfig ran for a refused import")
	}

	if _, err := e.h.ImportConfig(ctx, withPassphrase(importReq(archive), importTestPassphrase)); err != nil {
		t.Fatalf("the retry with the right passphrase: %v", err)
	}
}

// The data-loss scenario for the .env files: the stacks cannot be put in
// place after the database was restored. Every stack file, its .env
// included, is wholly as it was.
func TestImportConfig_AFailureAfterTheDatabaseRestoreLeavesEveryStackEnvAsItWas(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})

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
	if !strings.Contains(ae.message, "left as they were: app stacks") {
		t.Errorf("message = %q", ae.message)
	}
	if got := dirSnapshot(t, e.paths.StacksDir); !reflect.DeepEqual(got, stacksAtFailure) {
		t.Errorf("stacks changed although their apply failed:\nbefore %v\nafter  %v", stacksAtFailure, got)
	}
	if stacksAtFailure["web/.env"] != "file TOKEN=live" {
		t.Errorf("web/.env = %q before the failure, want the live one", stacksAtFailure["web/.env"])
	}
}

func TestImportConfig_ASymbolicLinkAsAStackEnvIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})
	outside := filepath.Join(t.TempDir(), "outside.env")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.paths.StacksDir, "web", ".env")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dbBefore := liveFingerprint(t, e.db)
	filesBefore := e.runtimeSnapshot(t)

	_, err := e.h.ImportConfig(ctx, importReq(archive))
	_ = importErr(t, err, 409, "restore_path_unsafe")
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
}

func TestPreviewConfigImport_ASymbolicLinkAsAStackEnvIsABlocker(t *testing.T) {
	ctx := context.Background()
	e := newImportFilesEnv(t)
	archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})
	outside := filepath.Join(t.TempDir(), "outside.env")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.paths.StacksDir, "web", ".env")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	p, err := e.h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: importReq(archive).Archive})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeRestorePathUnsafe {
		t.Fatalf("blockers = %+v, want the restore_path_unsafe the import would refuse with", p.Blockers)
	}
}

func TestPreviewConfigImport_ReportsWhetherTheSecretsWouldRestore(t *testing.T) {
	ctx := context.Background()
	previewReq := func(archive []byte, passphrase *string) *apiv1.PreviewConfigImportReq {
		req := &apiv1.PreviewConfigImportReq{Archive: importReq(archive).Archive}
		if passphrase != nil {
			req.Passphrase = apiv1.NewOptString(*passphrase)
		}
		return req
	}
	right, wrong := importTestPassphrase, "wrong passphrase"

	tests := []struct {
		name       string
		configured backup.SecretSource
		explicit   *string
		want       apiv1.ConfigImportSecretsStatus
		wantStacks []string
	}{
		{"configured passphrase", &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true}, nil, apiv1.ConfigImportSecretsStatusOpened, []string{}},
		{"explicit passphrase", &backup.FakeSecretSource{}, &right, apiv1.ConfigImportSecretsStatusOpened, []string{}},
		{"none", &backup.FakeSecretSource{}, nil, apiv1.ConfigImportSecretsStatusNoPassphrase, []string{"web"}},
		{"a wrong configured one", &backup.FakeSecretSource{Passphrase: "no", HasPass: true}, nil, apiv1.ConfigImportSecretsStatusPassphraseIncorrect, []string{"web"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportFilesEnv(t)
			archive := e.exportWithSecrets(t, tc.configured)
			dbBefore := liveFingerprint(t, e.db)
			filesBefore := e.runtimeSnapshot(t)

			p, err := e.h.PreviewConfigImport(ctx, previewReq(archive, tc.explicit))
			if err != nil {
				t.Fatalf("PreviewConfigImport: %v", err)
			}
			if p.Secrets.Status != tc.want || !reflect.DeepEqual(p.Secrets.Stacks, tc.wantStacks) {
				t.Errorf("secrets = %+v, want %s with stacks %v", p.Secrets, tc.want, tc.wantStacks)
			}
			if got := liveFingerprint(t, e.db); got != dbBefore {
				t.Error("the database changed")
			}
			if got := e.runtimeSnapshot(t); !reflect.DeepEqual(got, filesBefore) {
				t.Error("a runtime directory changed")
			}
		})
	}

	t.Run("an archive without a secrets section", func(t *testing.T) {
		e := newImportFilesEnv(t)
		archive := e.seedAndExport(t)
		p, err := e.h.PreviewConfigImport(ctx, previewReq(archive, nil))
		if err != nil {
			t.Fatal(err)
		}
		if p.Secrets.Status != apiv1.ConfigImportSecretsStatusNone || !reflect.DeepEqual(p.Secrets.Stacks, []string{"web"}) {
			t.Errorf("secrets = %+v", p.Secrets)
		}
	})

	t.Run("an explicit passphrase that does not open it is refused", func(t *testing.T) {
		e := newImportFilesEnv(t)
		archive := e.exportWithSecrets(t, &backup.FakeSecretSource{Passphrase: importTestPassphrase, HasPass: true})
		_, err := e.h.PreviewConfigImport(ctx, previewReq(archive, &wrong))
		_ = importErr(t, err, 400, "backup_passphrase_incorrect")
	})
}
