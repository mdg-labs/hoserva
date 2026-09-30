package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sealTestService(t *testing.T, configured *FakeSecretSource) (*Service, string) {
	t.Helper()
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups")
	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	return &Service{
		DB:           openTestDB(t),
		Paths:        paths,
		Secrets:      configured,
		Cipher:       FakeSecretCipher{},
		Destinations: []Destination{{ID: "local", Path: destDir, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}}},
		Hostname:     "test-host",
		Version:      "0.0.0-test",
		Now:          func() time.Time { return now },
	}, destDir
}

func writtenTree(t *testing.T, destDir string, a WrittenArchive) string {
	t.Helper()
	tree := t.TempDir()
	if err := unpackArchive(filepath.Join(destDir, a.Name), tree); err != nil {
		t.Fatalf("unpacking %s: %v", a.Name, err)
	}
	return tree
}

func TestRunReasonArchive_SealSecretsWith(t *testing.T) {
	ctx := context.Background()

	t.Run("with no passphrase configured the archive holds the stack .env under the given one", func(t *testing.T) {
		svc, dest := sealTestService(t, &FakeSecretSource{})
		a, err := svc.RunReasonArchive(ctx, ReasonPreImport, SealSecretsWith("import passphrase"))
		if err != nil {
			t.Fatalf("RunReasonArchive: %v", err)
		}
		if !a.SecretsSealed {
			t.Fatal("SecretsSealed = false")
		}
		s, err := ReadSecrets(writtenTree(t, dest, a), "import passphrase")
		if err != nil {
			t.Fatalf("opening secrets.age with the given passphrase: %v", err)
		}
		if got := s.StackEnvs(); len(got) != 1 || got[0].Stack != "plex" || string(got[0].Body) != "TOKEN=secret\n" {
			t.Fatalf("StackEnvs = %+v", got)
		}
	})

	t.Run("a configured passphrase does not seal it", func(t *testing.T) {
		svc, dest := sealTestService(t, &FakeSecretSource{Passphrase: "configured", HasPass: true})
		a, err := svc.RunReasonArchive(ctx, ReasonPreImport, SealSecretsWith("import passphrase"))
		if err != nil {
			t.Fatalf("RunReasonArchive: %v", err)
		}
		tree := writtenTree(t, dest, a)
		if _, err := ReadSecrets(tree, "import passphrase"); err != nil {
			t.Fatalf("opening secrets.age with the given passphrase: %v", err)
		}
		if _, err := ReadSecrets(tree, "configured"); !errors.Is(err, ErrPassphraseIncorrect) {
			t.Fatalf("ReadSecrets with the configured passphrase = %v, want ErrPassphraseIncorrect", err)
		}
	})

	t.Run("without the option nothing changes", func(t *testing.T) {
		svc, dest := sealTestService(t, &FakeSecretSource{})
		a, err := svc.RunReasonArchive(ctx, ReasonPreImport)
		if err != nil {
			t.Fatalf("RunReasonArchive: %v", err)
		}
		if a.SecretsSealed {
			t.Error("SecretsSealed = true with no passphrase")
		}
		if _, err := os.Stat(filepath.Join(writtenTree(t, dest, a), "secrets.age")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("secrets.age: %v, want none", err)
		}

		svc, dest = sealTestService(t, &FakeSecretSource{Passphrase: "configured", HasPass: true})
		a, err = svc.RunReasonArchive(ctx, ReasonPreImport)
		if err != nil {
			t.Fatalf("RunReasonArchive: %v", err)
		}
		if _, err := ReadSecrets(writtenTree(t, dest, a), "configured"); err != nil || !a.SecretsSealed {
			t.Fatalf("configured passphrase: %v, sealed %v", err, a.SecretsSealed)
		}
	})

	t.Run("a failure reading what to seal fails the run and writes nothing", func(t *testing.T) {
		svc, dest := sealTestService(t, &FakeSecretSource{})
		svc.Secrets = failingSecrets{}
		if _, err := svc.RunReasonArchive(ctx, ReasonPreImport, SealSecretsWith("import passphrase")); err == nil {
			t.Fatal("RunReasonArchive succeeded")
		}
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Fatalf("destination holds %v after a failed run", entries)
		}
	})
}

type failingSecrets struct{}

func (failingSecrets) BackupPassphrase(context.Context) (string, bool, error) { return "", false, nil }
func (failingSecrets) DatabaseSecrets(context.Context) ([]DatabaseSecret, error) {
	return nil, errors.New("secret store unavailable")
}

func TestSecretsOutcome_EnvPassphrase(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		tree        func(t *testing.T) string
		src         SecretSource
		explicit    *string
		wantPass    string
		wantRequest bool
		wantOK      bool
	}{
		{"opened by the request's", resolveTree, configured("", false), strPtr(testBackupPassphrase), testBackupPassphrase, true, true},
		{"opened by the request's over a wrong configured one", resolveTree, configured("wrong", true), strPtr(testBackupPassphrase), testBackupPassphrase, true, true},
		{"opened by the configured one", resolveTree, configured(testBackupPassphrase, true), nil, testBackupPassphrase, false, true},
		{"not opened", resolveTree, configured("wrong", true), nil, "", false, false},
		{"no secrets section", func(t *testing.T) string { return filesTree(t, archivedContent) }, configured("", false), strPtr("anything"), "", false, false},
		{"opened but holding no .env of a stack the archive has", func(t *testing.T) string {
			return secretsTree(t, archivedContent, testBackupPassphrase, map[string]string{"ghost": "X=1"})
		}, configured("", false), strPtr(testBackupPassphrase), "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ResolveSecrets(ctx, tc.tree(t), tc.src, tc.explicit)
			if err != nil {
				t.Fatalf("ResolveSecrets: %v", err)
			}
			pass, request, ok := out.EnvPassphrase()
			if pass != tc.wantPass || request != tc.wantRequest || ok != tc.wantOK {
				t.Fatalf("EnvPassphrase = (%q, %v, %v), want (%q, %v, %v)", pass, request, ok, tc.wantPass, tc.wantRequest, tc.wantOK)
			}
		})
	}
}
