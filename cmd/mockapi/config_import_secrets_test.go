package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"

	_ "modernc.org/sqlite"
)

const mockArchivePassphrase = "mock passphrase"

// mockSecretsArchive builds a real config archive whose secrets.age holds
// the .env of stack web, sealed under mockArchivePassphrase.
func mockSecretsArchive(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	stacks := filepath.Join(dir, "stacks")
	if err := os.MkdirAll(filepath.Join(stacks, "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"compose.yml": "services: {}\n", ".env": "TOKEN=1\n"} {
		if err := os.WriteFile(filepath.Join(stacks, "web", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	src := &backup.FakeSecretSource{Passphrase: mockArchivePassphrase, HasPass: true}
	if _, err := backup.BuildArchive(context.Background(), db, backup.Paths{StacksDir: stacks}, src, backup.FakeSecretCipher{}, "host", "test", time.Now(), staging); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func importUpload(archive []byte, passphrase *string) (*apiv1.ImportConfigReq, *apiv1.PreviewConfigImportReq) {
	imp := &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
	prev := &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
	if passphrase != nil {
		imp.Passphrase = apiv1.NewOptString(*passphrase)
		prev.Passphrase = apiv1.NewOptString(*passphrase)
	}
	return imp, prev
}

func TestMockConfigImport_TriesThePassphraseAsProductionDoes(t *testing.T) {
	ctx := context.Background()
	archive := mockSecretsArchive(t)
	right, wrong := mockArchivePassphrase, "wrong"

	tests := []struct {
		name       string
		configured string
		explicit   *string
		want       apiv1.ConfigImportSecretsStatus
		wantStacks []string
		wantEnvs   int64
		wantSealed apiv1.ConfigImportPreImportSecrets
	}{
		{"explicit passphrase", "", &right, apiv1.ConfigImportSecretsStatusOpened, []string{}, 1, apiv1.ConfigImportPreImportSecretsRequest},
		{"explicit passphrase over a configured one", "other", &right, apiv1.ConfigImportSecretsStatusOpened, []string{}, 1, apiv1.ConfigImportPreImportSecretsRequest},
		{"configured passphrase", mockArchivePassphrase, nil, apiv1.ConfigImportSecretsStatusOpened, []string{}, 1, apiv1.ConfigImportPreImportSecretsConfigured},
		{"no passphrase", "", nil, apiv1.ConfigImportSecretsStatusNoPassphrase, []string{"web"}, 0, apiv1.ConfigImportPreImportSecretsNone},
		{"a wrong configured passphrase", "other", nil, apiv1.ConfigImportSecretsStatusPassphraseIncorrect, []string{"web"}, 0, apiv1.ConfigImportPreImportSecretsConfigured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := newHandler("healthy")
			if err != nil {
				t.Fatal(err)
			}
			if tc.configured != "" {
				if _, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{BackupPassphrase: apiv1.NewOptString(tc.configured)}); err != nil {
					t.Fatal(err)
				}
			}
			imp, prev := importUpload(archive, tc.explicit)

			report, err := h.ImportConfig(ctx, imp)
			if err != nil {
				t.Fatalf("ImportConfig: %v", err)
			}
			if report.Secrets != tc.want {
				t.Errorf("report secrets = %s, want %s", report.Secrets, tc.want)
			}
			if report.PreImportSecrets != tc.wantSealed {
				t.Errorf("preImportSecrets = %s, want %s", report.PreImportSecrets, tc.wantSealed)
			}
			var envs int64
			for _, r := range report.Restored {
				if r.Category == apiv1.ConfigImportRestoredCategoryStackEnv {
					envs = r.Added
				}
			}
			if envs != tc.wantEnvs || (tc.wantEnvs == 0) != (len(report.NotRestored) == 1) {
				t.Errorf("stack_env added = %d, notRestored = %+v", envs, report.NotRestored)
			}

			preview, err := h.PreviewConfigImport(ctx, prev)
			if err != nil {
				t.Fatalf("PreviewConfigImport: %v", err)
			}
			if preview.Secrets.Status != tc.want || !reflect.DeepEqual(preview.Secrets.Stacks, tc.wantStacks) {
				t.Errorf("preview secrets = %+v, want %s %v", preview.Secrets, tc.want, tc.wantStacks)
			}
		})
	}

	t.Run("an explicit passphrase that does not open it is refused", func(t *testing.T) {
		h, err := newHandler("healthy")
		if err != nil {
			t.Fatal(err)
		}
		imp, prev := importUpload(archive, &wrong)
		_, importErr := h.ImportConfig(ctx, imp)
		_, previewErr := h.PreviewConfigImport(ctx, prev)
		for op, err := range map[string]error{"ImportConfig": importErr, "PreviewConfigImport": previewErr} {
			var me *mockError
			if !errors.As(err, &me) || me.code != "backup_passphrase_incorrect" || me.statusCode != 400 {
				t.Errorf("%s = %v, want 400 backup_passphrase_incorrect", op, err)
			}
		}
	})
}
