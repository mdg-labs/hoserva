package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func s3Request() NewDestination {
	return NewDestination{
		Name: "Backblaze", Type: TypeS3, Path: "bucket/hoserva",
		Options: map[string]string{"access_key_id": "AKIA", "provider": "Other", "endpoint": "https://s3.example.com"},
		Secrets: map[string]string{"secret_access_key": "s3cr3t-key"},
	}
}

type remoteRig struct {
	svc    *Service
	rclone *FakeRclone
	store  *FakeDestinationStore
	now    time.Time
	root   string
}

func newRemoteRig(t *testing.T) *remoteRig {
	t.Helper()
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	recipient, err := LoadOrGenerateRecipient(ctx, FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}
	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	rig := &remoteRig{
		rclone: &FakeRclone{Now: func() time.Time { return now }},
		store:  &FakeDestinationStore{},
		now:    now,
		root:   root,
	}
	rig.svc = &Service{
		DB:                db,
		Paths:             paths,
		Store:             rig.store,
		Rclone:            rig.rclone,
		Secrets:           &FakeSecretSource{Passphrase: "backup-pass", HasPass: true},
		Cipher:            FakeSecretCipher{},
		DestinationCipher: FakeSecretCipher{},
		Recipient:         recipient,
		Hostname:          "test-host",
		Version:           "0.0.0-test",
		Now:               func() time.Time { return rig.now },
	}
	return rig
}

func TestPrepareDestination_Validation(t *testing.T) {
	sftp := NewDestination{
		Name: "VPS", Type: TypeSFTP, Path: "/backups",
		Options: map[string]string{"host": "vps.example.com", "user": "hoserva"},
		Secrets: map[string]string{"pass": "pw"},
	}
	tests := []struct {
		name       string
		mutate     func(*NewDestination)
		base       NewDestination
		hasPass    bool
		existing   []Destination
		wantErr    error
		wantSubstr string
	}{
		{name: "s3 valid", base: s3Request(), hasPass: true},
		{name: "sftp valid with password", base: sftp, hasPass: true},
		{name: "sftp valid with key file", base: sftp, hasPass: true, mutate: func(n *NewDestination) {
			n.Secrets = nil
			n.Options = map[string]string{"host": "h", "user": "u", "key_file": "/root/.ssh/id"}
		}},
		{name: "sftp needs a credential", base: sftp, hasPass: true, mutate: func(n *NewDestination) { n.Secrets = nil }, wantErr: ErrInvalidDestination, wantSubstr: "pass or key_file"},
		{name: "smb valid", hasPass: true, base: NewDestination{
			Name: "NAS", Type: TypeSMB, Path: "share/backups",
			Options: map[string]string{"host": "nas", "user": "me"}, Secrets: map[string]string{"pass": "x"},
		}},
		{name: "smb needs a share", hasPass: true, base: NewDestination{
			Name: "NAS", Type: TypeSMB, Path: "",
			Options: map[string]string{"host": "nas", "user": "me"}, Secrets: map[string]string{"pass": "x"},
		}, wantErr: ErrInvalidDestination, wantSubstr: "share name"},
		{name: "webdav valid", hasPass: true, base: NewDestination{
			Name: "Cloud", Type: TypeWebDAV, Path: "hoserva",
			Options: map[string]string{"url": "https://dav.example.com", "user": "me", "vendor": "nextcloud"}, Secrets: map[string]string{"pass": "x"},
		}},
		{name: "rclone remote valid", hasPass: true, base: NewDestination{
			Name: "Mine", Type: TypeRclone, Path: "hoserva", Options: map[string]string{"remote": "gdrive"},
		}},
		{name: "rclone remote name injection", hasPass: true, base: NewDestination{
			Name: "Mine", Type: TypeRclone, Path: "hoserva", Options: map[string]string{"remote": "-o evil"},
		}, wantErr: ErrInvalidDestination},
		{name: "remote needs a passphrase", base: s3Request(), hasPass: false, wantErr: ErrPassphraseRequired},
		{name: "remote is always encrypted", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Encrypt = boolPtr(false) }, wantErr: ErrInvalidDestination, wantSubstr: "always encrypted"},
		{name: "unknown option key", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Options["config_file"] = "/etc/shadow" }, wantErr: ErrInvalidDestination, wantSubstr: "config_file"},
		{name: "unknown secret key", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Secrets["token"] = "x" }, wantErr: ErrInvalidDestination, wantSubstr: "token"},
		{name: "missing required option", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { delete(n.Options, "access_key_id") }, wantErr: ErrInvalidDestination, wantSubstr: "access_key_id"},
		{name: "missing required secret", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Secrets = nil }, wantErr: ErrInvalidDestination, wantSubstr: "secret_access_key"},
		{name: "control character in option", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Options["endpoint"] = "https://x\nRCLONE_CONFIG_X=1" }, wantErr: ErrInvalidDestination, wantSubstr: "control"},
		{name: "path escapes with dotdot", base: s3Request(), hasPass: true, mutate: func(n *NewDestination) { n.Path = "bucket/../other" }, wantErr: ErrInvalidDestination, wantSubstr: ".."},
		{name: "local valid", hasPass: false, base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/mnt/disks/backup"}},
		{name: "local opt-in encryption", hasPass: true, base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/mnt/disks/backup", Encrypt: boolPtr(true)}},
		{name: "local relative path", base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "backups"}, wantErr: ErrInvalidDestination, wantSubstr: "absolute"},
		{name: "local root", base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/"}, wantErr: ErrInvalidDestination, wantSubstr: "root"},
		{name: "local with options", base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/x", Options: map[string]string{"host": "h"}}, wantErr: ErrInvalidDestination},
		{name: "unknown type", base: NewDestination{Name: "Disk", Type: "ftp", Path: "/x"}, wantErr: ErrInvalidDestination, wantSubstr: "type"},
		{name: "empty name", base: NewDestination{Name: "  ", Type: TypeLocal, Path: "/x"}, wantErr: ErrInvalidDestination},
		{name: "retention keeps nothing", base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/x", Retention: &Retention{}}, wantErr: ErrInvalidDestination, wantSubstr: "at least one"},
		{name: "retention out of range", base: NewDestination{Name: "Disk", Type: TypeLocal, Path: "/x", Retention: &Retention{Daily: -1}}, wantErr: ErrInvalidDestination},
		{name: "duplicate name", base: NewDestination{Name: "boot device", Type: TypeLocal, Path: "/x"},
			existing: []Destination{{ID: "boot", Name: "Boot device", Type: TypeLocal, Path: "/var/lib/hoserva/backups"}}, wantErr: ErrDestinationExists},
		{name: "duplicate local path", base: NewDestination{Name: "Other", Type: TypeLocal, Path: "/mnt/disks/backup/"},
			existing: []Destination{{ID: "a", Name: "A", Type: TypeLocal, Path: "/mnt/disks/backup"}}, wantErr: ErrDestinationExists},
		{name: "same path on a different remote is fine", hasPass: true, base: s3Request(),
			existing: []Destination{{ID: "a", Name: "A", Type: TypeS3, Path: "bucket/hoserva", Options: map[string]string{"access_key_id": "OTHER"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nd := tc.base
			if nd.Options != nil {
				nd.Options = copyMap(nd.Options)
			}
			if nd.Secrets != nil {
				nd.Secrets = copyMap(nd.Secrets)
			}
			if tc.mutate != nil {
				tc.mutate(&nd)
			}
			got, err := PrepareDestination(nd, tc.existing, tc.hasPass)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("PrepareDestination: %v", err)
				}
				if got.isRemote() && !got.Encrypt {
					t.Fatal("a remote destination must always encrypt")
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestAddDestination_RcloneMissingReportsInstallCommandAndStoresNothing(t *testing.T) {
	rig := newRemoteRig(t)
	rig.rclone.Missing = true

	_, err := rig.svc.AddDestination(context.Background(), s3Request())
	if !errors.Is(err, ErrRcloneMissing) {
		t.Fatalf("err = %v, want ErrRcloneMissing", err)
	}
	if !strings.Contains(err.Error(), "sudo apt install rclone") {
		t.Fatalf("err = %q, want it to carry the install command", err)
	}
	if dests, _ := rig.store.ListDestinations(context.Background()); len(dests) != 0 {
		t.Fatalf("a refused destination was stored: %+v", dests)
	}
}

func TestAddDestination_LocalNeverNeedsRclone(t *testing.T) {
	rig := newRemoteRig(t)
	rig.rclone.Missing = true

	dest, err := rig.svc.AddDestination(context.Background(), NewDestination{Name: "Disk", Type: TypeLocal, Path: filepath.Join(rig.root, "disk")})
	if err != nil {
		t.Fatalf("AddDestination: %v", err)
	}
	if dest.ID == "" || dest.CreatedAt.IsZero() {
		t.Fatalf("destination is missing its id or creation time: %+v", dest)
	}
	if calls := rig.rclone.Calls(); len(calls) != 0 {
		t.Fatalf("a local destination ran rclone: %+v", calls)
	}
}

func TestAddDestination_SealsCredentialsAndRefusesRemoteWithoutPassphrase(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()

	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatalf("AddDestination: %v", err)
	}
	if !dest.Encrypt || len(dest.SealedSecrets) == 0 || dest.Secrets != nil {
		t.Fatalf("destination = %+v, want it encrypting with sealed, not plain, credentials", dest)
	}
	if bytes.Contains(dest.SealedSecrets, []byte("s3cr3t-key")) {
		t.Fatal("the stored credentials contain the secret in plain text")
	}
	stored, err := rig.store.GetDestination(ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.SealedSecrets, []byte("s3cr3t-key")) || len(stored.Options) != 3 {
		t.Fatalf("stored = %+v", stored)
	}

	rig.svc.Secrets = &FakeSecretSource{HasPass: false}
	other := s3Request()
	other.Name = "Second"
	other.Path = "bucket/second"
	if _, err := rig.svc.AddDestination(ctx, other); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("err = %v, want ErrPassphraseRequired", err)
	}
}

func TestSeedDestinations_OnlyWhenEmpty(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	defaults := DefaultDestinations()

	if err := rig.svc.SeedDestinations(ctx, defaults); err != nil {
		t.Fatalf("SeedDestinations: %v", err)
	}
	got, _ := rig.store.ListDestinations(ctx)
	if len(got) != 2 || got[0].ID != "boot" || got[1].ID != "pool" || !got[0].Enabled || !got[1].Enabled {
		t.Fatalf("seeded = %+v, want boot and pool enabled", got)
	}

	if err := rig.svc.RemoveDestination(ctx, "pool"); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.SeedDestinations(ctx, defaults); err != nil {
		t.Fatal(err)
	}
	got, _ = rig.store.ListDestinations(ctx)
	if len(got) != 1 {
		t.Fatalf("a destination the operator removed came back: %+v", got)
	}
}

func remoteArchiveNames(rig *remoteRig) []string {
	var names []string
	for key := range rig.rclone.Files() {
		names = append(names, key)
	}
	return names
}

func TestRun_WritesEncryptedArchiveToRemoteThroughRcloneCopy(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatalf("AddDestination: %v", err)
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	name := archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0)
	files := rig.rclone.Files()
	archiveKey := "HOSERVADEST:bucket/hoserva/" + name + ".age"
	if _, ok := files[archiveKey]; !ok {
		t.Fatalf("encrypted archive missing from the remote; remote has %v", remoteArchiveNames(rig))
	}
	if _, ok := files[archiveKey+identitySidecarSuffix]; !ok {
		t.Fatalf("identity sidecar missing from the remote; remote has %v", remoteArchiveNames(rig))
	}
	for key := range files {
		if strings.HasSuffix(key, ".tar.zst") {
			t.Fatalf("a plaintext archive reached the remote: %s", key)
		}
	}

	var copies int
	for _, c := range rig.rclone.Calls() {
		switch c.Args[0] {
		case "copy":
			copies++
			want := []string{"copy", "--immutable"}
			if len(c.Args) != 4 || c.Args[0] != want[0] || c.Args[1] != want[1] || c.Args[3] != "HOSERVADEST:bucket/hoserva" || !filepath.IsAbs(c.Args[2]) {
				t.Fatalf("copy argv = %q", c.Args)
			}
		case "obscure", "version", "lsjson":
		default:
			t.Fatalf("unexpected rclone subcommand %q in %q", c.Args[0], c.Args)
		}
		for _, a := range c.Args {
			if strings.Contains(a, "s3cr3t-key") {
				t.Fatalf("a credential reached rclone's argv: %q", c.Args)
			}
		}
	}
	if copies != 2 {
		t.Fatalf("rclone copy ran %d times, want 2 (sidecar and archive)", copies)
	}

	stored, err := rig.store.GetDestination(ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastSuccessfulBackupAt == nil || !stored.LastSuccessfulBackupAt.Equal(rig.now) {
		t.Fatalf("LastSuccessfulBackupAt = %v, want %v", stored.LastSuccessfulBackupAt, rig.now)
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestRun_ConfiguresRemoteThroughEnvironmentWithObscuredPassword(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if _, err := rig.svc.AddDestination(ctx, NewDestination{
		Name: "NAS", Type: TypeSMB, Path: "share/backups",
		Options: map[string]string{"host": "nas.lan", "user": "me"},
		Secrets: map[string]string{"pass": "hunter2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var copyCall *RcloneCommand
	for _, c := range rig.rclone.Calls() {
		if c.Args[0] == "copy" {
			c := c
			copyCall = &c
			break
		}
	}
	if copyCall == nil {
		t.Fatal("rclone copy never ran")
	}
	for key, want := range map[string]string{
		"RCLONE_CONFIG_HOSERVADEST_TYPE": "smb",
		"RCLONE_CONFIG_HOSERVADEST_HOST": "nas.lan",
		"RCLONE_CONFIG_HOSERVADEST_USER": "me",
		"RCLONE_CONFIG_HOSERVADEST_PASS": "obscured-hunter2",
	} {
		if got, ok := envValue(copyCall.Env, key); !ok || got != want {
			t.Fatalf("%s = %q (present %v), want %q; env = %q", key, got, ok, want, copyCall.Env)
		}
	}
	for _, a := range copyCall.Args {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("the password reached argv: %q", copyCall.Args)
		}
	}
}

func TestRun_RcloneRemoteDestinationUsesTheNamedRemote(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if _, err := rig.svc.AddDestination(ctx, NewDestination{
		Name: "Mine", Type: TypeRclone, Path: "hoserva", Options: map[string]string{"remote": "gdrive"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := rig.rclone.Files()["gdrive:hoserva/"+archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0)+".age"]; !ok {
		t.Fatalf("archive missing from gdrive:hoserva; remote has %v", remoteArchiveNames(rig))
	}
}

func TestRun_NeverWritesUnencryptedToARemoteDestination(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatal(err)
	}
	// A row that lost its encrypt flag, however it got that way, must not
	// be written unencrypted (Q80).
	if err := rig.store.update(dest.ID, func(d *Destination) { d.Encrypt = false }); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(rig.root, "local")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: local}); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range rig.rclone.Calls() {
		if c.Args[0] == "copy" {
			t.Fatalf("rclone copy ran for an unencrypted remote destination: %q", c.Args)
		}
	}
	if _, err := os.Stat(filepath.Join(local, archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0))); err != nil {
		t.Fatalf("the local destination was not written: %v", err)
	}
	stored, _ := rig.store.GetDestination(ctx, dest.ID)
	if stored.LastSuccessfulBackupAt != nil {
		t.Fatal("a refused write was recorded as a success")
	}
}

func TestRun_RemoteFailureDoesNotBlockLocalAndIsNotRecordedAsSuccess(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	remote, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatal(err)
	}
	local, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: filepath.Join(rig.root, "local")})
	if err != nil {
		t.Fatal(err)
	}
	rig.rclone.Fail = map[string]error{"copy": &RcloneExitError{Code: 1, Output: "access denied"}}
	var logged []string
	rig.svc.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v — a destination written must make the run succeed", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "access denied") {
		t.Fatalf("logged = %q, want the remote failure", logged)
	}
	r, _ := rig.store.GetDestination(ctx, remote.ID)
	l, _ := rig.store.GetDestination(ctx, local.ID)
	if r.LastSuccessfulBackupAt != nil {
		t.Fatal("the failed remote was recorded as a success")
	}
	if l.LastSuccessfulBackupAt == nil {
		t.Fatal("the local destination's success was not recorded")
	}
}

func TestRun_FailsWhenTheOnlyDestinationFails(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if _, err := rig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	rig.rclone.Fail = map[string]error{"copy": &RcloneExitError{Code: 1, Output: "access denied"}}

	err := rig.svc.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v, want the destination's failure", err)
	}
}

func TestRun_RemoteRetentionPrunesOldArchivesAndSidecars(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	retention := &Retention{Daily: 2, Weekly: 0, Monthly: 0}
	req := s3Request()
	req.Retention = retention
	if _, err := rig.svc.AddDestination(ctx, req); err != nil {
		t.Fatal(err)
	}
	dir := "HOSERVADEST:bucket/hoserva"
	for i := 1; i <= 4; i++ {
		day := rig.now.AddDate(0, 0, -i)
		name := fmt.Sprintf("hoserva-config-%s-%s.tar.zst.age", rig.svc.installationID(), day.Format("2006-01-02T15-04-05"))
		rig.rclone.Put(dir, name, []byte("old"), day)
		rig.rclone.Put(dir, name+identitySidecarSuffix, []byte("sidecar"), day)
	}
	rig.rclone.Put(dir, "unrelated.txt", []byte("mine"), rig.now.AddDate(-1, 0, 0))

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	files := rig.rclone.Files()
	var archives []string
	for key := range files {
		if strings.HasSuffix(key, ".tar.zst.age") {
			archives = append(archives, key)
		}
	}
	if len(archives) != 2 {
		t.Fatalf("archives kept = %v, want the new one and the newest old one", archives)
	}
	for key := range files {
		if strings.HasSuffix(key, identitySidecarSuffix) && !contains(archives, strings.TrimSuffix(key, identitySidecarSuffix)) {
			t.Fatalf("orphaned sidecar left behind: %s", key)
		}
	}
	if _, ok := files[dir+"/unrelated.txt"]; !ok {
		t.Fatal("pruning removed a file that is not one of Hoserva's archives")
	}
}

func TestTestDestination_LocalWritesReadsBackAndDeletes(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dir := filepath.Join(rig.root, "probe")
	dest, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Disk", Type: TypeLocal, Path: dir})
	if err != nil {
		t.Fatal(err)
	}

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil || !res.Success {
		t.Fatalf("TestDestination = %+v, %v", res, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the test file was left behind: %v", entries)
	}
	if calls := rig.rclone.Calls(); len(calls) != 0 {
		t.Fatalf("a local test ran rclone: %+v", calls)
	}
}

func TestTestDestination_LocalReportsAnUnwritablePath(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	blocker := filepath.Join(rig.root, "blocker")
	if err := os.WriteFile(blocker, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Disk", Type: TypeLocal, Path: filepath.Join(blocker, "sub")})
	if err != nil {
		t.Fatal(err)
	}

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil {
		t.Fatalf("TestDestination: %v", err)
	}
	if res.Success || res.Error == "" {
		t.Fatalf("result = %+v, want a failure with a reason", res)
	}
}

func TestTestDestination_RemoteWritesReadsBackAndDeletes(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatal(err)
	}
	before := len(rig.rclone.Calls())

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil || !res.Success {
		t.Fatalf("TestDestination = %+v, %v", res, err)
	}
	var subs []string
	for _, c := range rig.rclone.Calls()[before:] {
		subs = append(subs, c.Args[0])
	}
	if !containsInOrder(subs, "copy", "cat", "deletefile") {
		t.Fatalf("rclone subcommands = %v, want copy, then cat, then deletefile", subs)
	}
	if files := rig.rclone.Files(); len(files) != 0 {
		t.Fatalf("the test file was left on the remote: %v", remoteArchiveNames(rig))
	}
}

func containsInOrder(list []string, want ...string) bool {
	i := 0
	for _, s := range list {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestTestDestination_RemoteReadFailureStillDeletesTheFile(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatal(err)
	}
	rig.rclone.Fail = map[string]error{"cat": &RcloneExitError{Code: 1, Output: "read failed"}}

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil {
		t.Fatalf("TestDestination: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "read failed") {
		t.Fatalf("result = %+v, want the read failure", res)
	}
	if files := rig.rclone.Files(); len(files) != 0 {
		t.Fatalf("the test file was left on the remote: %v", remoteArchiveNames(rig))
	}
}

func TestTestDestination_RemoteWithoutRcloneIsAnError(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, s3Request())
	if err != nil {
		t.Fatal(err)
	}
	rig.rclone.Missing = true

	if _, err := rig.svc.TestDestination(ctx, dest.ID); !errors.Is(err, ErrRcloneMissing) {
		t.Fatalf("err = %v, want ErrRcloneMissing", err)
	}
}

func TestTestDestination_UnknownID(t *testing.T) {
	rig := newRemoteRig(t)
	if _, err := rig.svc.TestDestination(context.Background(), "nope"); !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("err = %v, want ErrDestinationNotFound", err)
	}
}

func TestTestDestination_PoolDestinationIsNotWrittenWhileUnmounted(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	poolRoot := filepath.Join(rig.root, "mnt", "user")
	rig.svc.PoolRoot = poolRoot
	rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
	dest, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Pool", Type: TypeLocal, Path: filepath.Join(poolRoot, "hoserva-backups")})
	if err != nil {
		t.Fatal(err)
	}

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.Contains(res.Error, "not mounted") {
		t.Fatalf("result = %+v, want a refusal because the pool is not mounted", res)
	}
	if _, err := os.Stat(poolRoot); !os.IsNotExist(err) {
		t.Fatalf("the test created %q on the boot device while the pool was unmounted: %v", poolRoot, err)
	}
}

func TestCheckStaleDestinations(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	created := rig.now.Add(-30 * 24 * time.Hour)
	recent := rig.now.Add(-6 * time.Hour)
	old := rig.now.Add(-72 * time.Hour)
	for _, d := range []Destination{
		{ID: "fresh", Name: "Fresh", Type: TypeLocal, Path: "/a", Enabled: true, CreatedAt: created, LastSuccessfulBackupAt: &recent},
		{ID: "stale", Name: "Stale", Type: TypeLocal, Path: "/b", Enabled: true, CreatedAt: created, LastSuccessfulBackupAt: &old},
		{ID: "never", Name: "Never", Type: TypeLocal, Path: "/c", Enabled: true, CreatedAt: created},
		{ID: "newborn", Name: "Newborn", Type: TypeLocal, Path: "/d", Enabled: true, CreatedAt: rig.now.Add(-time.Hour)},
		{ID: "off", Name: "Off", Type: TypeLocal, Path: "/e", Enabled: false, CreatedAt: created},
	} {
		if err := rig.store.CreateDestination(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	var alerted []string
	alert := func(_ context.Context, name string, _ *time.Time) error {
		alerted = append(alerted, name)
		return nil
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now, alert); err != nil {
		t.Fatal(err)
	}
	if len(alerted) != 2 || !contains(alerted, "Stale") || !contains(alerted, "Never") {
		t.Fatalf("alerted = %v, want Stale and Never only", alerted)
	}

	alerted = nil
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now.Add(time.Minute), alert); err != nil {
		t.Fatal(err)
	}
	if len(alerted) != 0 {
		t.Fatalf("the same staleness alerted again on the next tick: %v", alerted)
	}

	// A successful backup ends the staleness, so the next one alerts anew.
	if err := rig.store.RecordBackupSuccess(ctx, "stale", rig.now); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now.Add(72*time.Hour), alert); err != nil {
		t.Fatal(err)
	}
	if !contains(alerted, "Stale") {
		t.Fatalf("alerted = %v, want Stale again after it went stale a second time", alerted)
	}
	if calls := rig.rclone.Calls(); len(calls) != 0 {
		t.Fatalf("the stale check touched a destination: %+v", calls)
	}
}

func TestCheckStaleDestinations_SuccessDuringTheAlertKeepsTheNextStalenessAlerting(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if err := rig.store.CreateDestination(ctx, Destination{
		ID: "stale", Name: "Stale", Type: TypeLocal, Path: "/b", Enabled: true, CreatedAt: rig.now.Add(-100 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// A backup succeeds after the check read the row but before it marks
	// the alert sent.
	raced := false
	alert := func(ctx context.Context, _ string, _ *time.Time) error {
		if !raced {
			raced = true
			return rig.store.RecordBackupSuccess(ctx, "stale", rig.now)
		}
		return nil
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now, alert); err != nil {
		t.Fatalf("a success racing the mark is a state change, not a failure: %v", err)
	}
	got, err := rig.store.GetDestination(ctx, "stale")
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAlertedAt != nil {
		t.Fatalf("the healthy destination was marked alerted over the racing success: %+v", got)
	}

	var alerted []string
	alert = func(_ context.Context, name string, _ *time.Time) error {
		alerted = append(alerted, name)
		return nil
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now.Add(72*time.Hour), alert); err != nil {
		t.Fatal(err)
	}
	if !contains(alerted, "Stale") {
		t.Fatalf("alerted = %v, want Stale once it went stale again", alerted)
	}
}

func TestCheckStaleDestinations_RetriesAFailedAlert(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if err := rig.store.CreateDestination(ctx, Destination{
		ID: "stale", Name: "Stale", Type: TypeLocal, Path: "/b", Enabled: true, CreatedAt: rig.now.Add(-100 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	fail := true
	var sent int
	alert := func(context.Context, string, *time.Time) error {
		if fail {
			return errors.New("notification service down")
		}
		sent++
		return nil
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now, alert); err == nil {
		t.Fatal("a failed alert was reported as success")
	}
	fail = false
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now.Add(time.Minute), alert); err != nil || sent != 1 {
		t.Fatalf("retry: err = %v, sent = %d, want the alert sent once", err, sent)
	}
}

func TestRun_LocalDestinationsStillWorkWithoutRclone(t *testing.T) {
	rig := newRemoteRig(t)
	rig.rclone.Missing = true
	ctx := context.Background()
	dir := filepath.Join(rig.root, "local")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: dir}); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0))); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
}

func TestPrepareDestination_RefusesSystemLocations(t *testing.T) {
	for _, path := range []string{
		"/etc", "/etc/hoserva", "/usr", "/usr/local/backups", "/boot", "/proc/1", "/sys/kernel", "/dev", "/dev/shm", "/run", "/run/hoserva", "/tmp", "/var", "/tmp/../etc",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := PrepareDestination(NewDestination{Name: "Sys", Type: TypeLocal, Path: path}, nil, false)
			if !errors.Is(err, ErrInvalidDestination) {
				t.Fatalf("PrepareDestination(%q) err = %v, want ErrInvalidDestination", path, err)
			}
		})
	}
	for _, path := range []string{"/tmp/backups", "/var/backups/hoserva", "/mnt/user/Backups", "/mnt/disks/usb"} {
		t.Run("allows "+path, func(t *testing.T) {
			if _, err := PrepareDestination(NewDestination{Name: "Ok", Type: TypeLocal, Path: path}, nil, false); err != nil {
				t.Fatalf("PrepareDestination(%q): %v", path, err)
			}
		})
	}
}

func TestPrepareDestination_RefusesASymlinkIntoASystemLocation(t *testing.T) {
	link := filepath.Join(t.TempDir(), "etc-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "not-yet-created")} {
		_, err := PrepareDestination(NewDestination{Name: "Link", Type: TypeLocal, Path: path}, nil, false)
		if !errors.Is(err, ErrInvalidDestination) {
			t.Fatalf("PrepareDestination(%q) err = %v, want ErrInvalidDestination", path, err)
		}
	}
}

func TestPrepareDestination_RefusesTheDaemonsOwnDirectories(t *testing.T) {
	state, config := "/srv/hoserva-state", "/srv/hoserva-etc"
	for _, path := range []string{state, filepath.Join(state, "stacks"), config, filepath.Join(config, "shares")} {
		_, err := PrepareDestination(NewDestination{Name: "Own", Type: TypeLocal, Path: path}, nil, false, state, config)
		if !errors.Is(err, ErrInvalidDestination) {
			t.Fatalf("PrepareDestination(%q) err = %v, want ErrInvalidDestination", path, err)
		}
	}
	for _, path := range []string{filepath.Join(state, "backups"), "/srv/other"} {
		if _, err := PrepareDestination(NewDestination{Name: "Ok", Type: TypeLocal, Path: path}, nil, false, state, config); err != nil {
			t.Fatalf("PrepareDestination(%q): %v", path, err)
		}
	}
}

func TestAddDestination_RefusesTheServicesOwnStateAndConfigDirectories(t *testing.T) {
	rig := newRemoteRig(t)
	for _, path := range []string{rig.svc.Paths.StateDir, rig.svc.Paths.ConfigRoot} {
		_, err := rig.svc.AddDestination(context.Background(), NewDestination{Name: "Own " + path, Type: TypeLocal, Path: path})
		if !errors.Is(err, ErrInvalidDestination) {
			t.Fatalf("AddDestination(%q) err = %v, want ErrInvalidDestination", path, err)
		}
	}
}

func TestLocalDestination_NeverChangesAnExistingDirectoryMode(t *testing.T) {
	for name, mode := range map[string]os.FileMode{
		"share":  0o775,
		"sticky": os.ModeSticky | 0o777,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newRemoteRig(t)
			ctx := context.Background()
			dir := filepath.Join(rig.root, "existing-"+name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			dest, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Share", Type: TypeLocal, Path: dir})
			if err != nil {
				t.Fatal(err)
			}
			assertMode := func(step string) {
				t.Helper()
				info, err := os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode() & (os.ModePerm | os.ModeSticky); got != mode {
					t.Fatalf("after %s the directory mode is %v, want the untouched %v", step, got, mode)
				}
			}

			if res, err := rig.svc.TestDestination(ctx, dest.ID); err != nil || !res.Success {
				t.Fatalf("TestDestination = %+v, %v", res, err)
			}
			assertMode("a connection test")
			if err := rig.svc.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertMode("a backup")
		})
	}
}

func TestLocalDestination_CreatesAMissingDirectoryPrivate(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dir := filepath.Join(rig.root, "new", "nested")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "New", Type: TypeLocal, Path: dir}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("created directory mode = %v, want 0700", info.Mode().Perm())
	}
}

// Two installations pointed at one directory (one NFS mount, one storage
// box) each prune only what they wrote.
func TestRetention_TwoInstallationsSharingADirectoryPruneOnlyTheirOwn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	recipientA, recipientB := newTestRecipient(t), newTestRecipient(t)

	dayOne := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	a1 := newSameDestinationService(t, recipientA, dir, "box-a", dayOne)
	b1 := newSameDestinationService(t, recipientB, dir, "box-b", dayOne.Add(12*time.Second))
	a2 := newSameDestinationService(t, recipientA, dir, "box-a", dayTwo)
	b2 := newSameDestinationService(t, recipientB, dir, "box-b", dayTwo.Add(12*time.Second))
	for _, svc := range []*Service{a1, b1, a2, b2} {
		for i := range svc.Destinations {
			svc.Destinations[i].Retention = Retention{Daily: 1}
		}
	}
	if a1.installationID() == b1.installationID() {
		t.Fatal("two installations derived the same id")
	}

	for _, svc := range []*Service{a1, b1} {
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	nameA := archiveName(a1.installationID(), dayOne, ReasonNone, 0)
	nameB := archiveName(b1.installationID(), dayOne.Add(12*time.Second), ReasonNone, 0)
	for _, name := range []string{nameA, nameB} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("archive %s missing after the first night: %v", name, err)
		}
	}

	// Retention keeps one archive per installation: B's second night must
	// prune B's first archive and leave A's alone, and A's the reverse.
	if err := b2.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, nameA)); err != nil {
		t.Fatalf("box B's retention removed box A's archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, nameB)); !os.IsNotExist(err) {
		t.Fatalf("box B kept its own expired archive: %v", err)
	}
	if err := a2.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, nameA)); !os.IsNotExist(err) {
		t.Fatalf("box A kept its own expired archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, archiveName(b2.installationID(), dayTwo.Add(12*time.Second), ReasonNone, 0))); err != nil {
		t.Fatalf("box A's retention removed box B's newest archive: %v", err)
	}
}

func TestRetention_ARemoteSharedWithAnotherInstallationIsLeftAlone(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	req := s3Request()
	req.Retention = &Retention{Daily: 1}
	if _, err := rig.svc.AddDestination(ctx, req); err != nil {
		t.Fatal(err)
	}
	dir := "HOSERVADEST:bucket/hoserva"
	other := "hoserva-config-ffffffffffff-2026-09-14T03-00-00.tar.zst.age"
	legacy := "hoserva-config-2026-09-10T03-00-00.tar.zst.age"
	for _, name := range []string{other, legacy} {
		rig.rclone.Put(dir, name, []byte("theirs"), rig.now.Add(-time.Minute))
		rig.rclone.Put(dir, name+identitySidecarSuffix, []byte("sidecar"), rig.now.Add(-time.Minute))
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := rig.rclone.Files()
	for _, name := range []string{other, other + identitySidecarSuffix, legacy, legacy + identitySidecarSuffix} {
		if _, ok := files[dir+"/"+name]; !ok {
			t.Fatalf("retention removed %s, which this installation did not write", name)
		}
	}
}

// A legacy archive is one an earlier release wrote to the boot or pool
// default, where nothing else writes; on any other destination it is not
// provably this installation's, so it is left alone.
func TestRetention_LegacyArchivesArePrunedOnlyOnTheDefaultDestinations(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id          string
		wantRemoved bool
	}{{DefaultBootID, true}, {DefaultPoolID, true}, {"dest-0123456789abcdef", false}} {
		t.Run(tc.id, func(t *testing.T) {
			dir := t.TempDir()
			svc := newSameDestinationService(t, newTestRecipient(t), dir, "box", now)
			svc.Destinations[0].ID = tc.id
			svc.Destinations[0].Retention = Retention{Daily: 1}
			legacy := filepath.Join(dir, "hoserva-config-2026-09-10T03-00.tar.zst")
			if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := now.AddDate(0, 0, -4)
			if err := os.Chtimes(legacy, old, old); err != nil {
				t.Fatal(err)
			}

			if err := svc.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			_, err := os.Stat(legacy)
			if removed := os.IsNotExist(err); removed != tc.wantRemoved {
				t.Fatalf("legacy archive removed = %v (stat: %v), want %v", removed, err, tc.wantRemoved)
			}
		})
	}
}
