package main

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testConfigImportPreview() *apiv1.ConfigImportPreview {
	return &apiv1.ConfigImportPreview{
		Archive: apiv1.ConfigImportArchive{
			Timestamp: time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC), Host: "nas", HoservaVersion: "1.2.3", SchemaVersion: "20260901000000",
		},
		LiveSchemaVersion: "20260901000000",
		Secrets:           apiv1.ConfigImportSecrets{Status: apiv1.ConfigImportSecretsStatusNone, Stacks: []string{"web"}},
		Blockers: []apiv1.ConfigImportBlocker{{
			Code: apiv1.ConfigImportBlockerCodeArchiveArrayMismatch, Message: "the archive's array differs from the live array",
		}},
		Groups: []apiv1.ConfigImportGroup{
			{
				Category: apiv1.ConfigImportGroupCategoryShares,
				Added:    []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShare, Name: "photos"}},
				Changed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShareUserPermission, Name: "media / alice"}},
				Removed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShare, Name: "scratch"}},
			},
			{
				Category: apiv1.ConfigImportGroupCategorySystem,
				Changed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindUps}},
			},
			{Category: apiv1.ConfigImportGroupCategorySchedules},
			{
				Category: apiv1.ConfigImportGroupCategoryCustomConfig,
				Changed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindCustomConfigFile, Name: "smb.custom.conf"}},
			},
			{
				Category: apiv1.ConfigImportGroupCategoryTemplates,
				Added:    []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindTemplateFile, Name: "plex/template.json"}},
			},
			{
				Category: apiv1.ConfigImportGroupCategoryStacks,
				Removed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindStackFile, Name: "web/compose.yml"}},
			},
		},
		Notes: []apiv1.ConfigImportNote{{Code: apiv1.ConfigImportNoteCodeSessionsReplaced, Message: "Active sign-in sessions are replaced by the archive's."}},
	}
}

func writeArchiveFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hoserva-config.tar.zst")
	if err := os.WriteFile(path, []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigImportPreview_SendsTheArchiveWithoutConfirmAndPrintsTheGroups(t *testing.T) {
	var gotRequest, gotArchive, gotFields string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading part: %v", err)
				return
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "archive" {
				gotArchive = string(b)
			} else {
				gotFields += part.FormName() + ";"
			}
		}
		writeJSON(t, w, http.StatusOK, testConfigImportPreview())
	})

	printed, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview: %v", err)
	}
	if gotRequest != "POST /api/v1/config/import/preview" {
		t.Fatalf("request = %q, want POST /api/v1/config/import/preview", gotRequest)
	}
	if gotArchive != "archive bytes" || gotFields != "" {
		t.Fatalf("archive = %q and other fields = %q, want the archive alone (no confirm)", gotArchive, gotFields)
	}
	for _, want := range []string{
		"nas", "1.2.3",
		"Import would be refused (archive_array_mismatch): the archive's array differs from the live array",
		"Shares and share permissions", "+ share: photos", "~ share user permission: media / alice", "- share: scratch",
		"System settings", "~ ups",
		"Custom config files", "~ custom config file: smb.custom.conf",
		"App templates", "+ template file: plex/template.json",
		"App stacks", "- stack file: web/compose.yml",
		"Active sign-in sessions are replaced by the archive's.",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "Schedules") || strings.Contains(printed, "No changes") {
		t.Errorf("output lists a category with no changes, or claims none:\n%s", printed)
	}
}

func TestConfigImportPreview_JSONOutputIsTheResponse(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, testConfigImportPreview())
	})
	printed, err := runAppCLI(t, sock, "--json", "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview --json: %v", err)
	}
	var got apiv1.ConfigImportPreview
	if err := json.Unmarshal([]byte(printed), &got); err != nil {
		t.Fatalf("output is not the preview's JSON: %v\n%s", err, printed)
	}
	if len(got.Groups) != 6 || got.Archive.Host != "nas" {
		t.Fatalf("decoded preview = %+v", got)
	}
}

func TestConfigImportPreview_ReportsAnUnchangedArchive(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		p := testConfigImportPreview()
		p.Blockers = []apiv1.ConfigImportBlocker{}
		p.Groups = []apiv1.ConfigImportGroup{{Category: apiv1.ConfigImportGroupCategoryShares}}
		writeJSON(t, w, http.StatusOK, p)
	})
	printed, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview: %v", err)
	}
	if !strings.Contains(printed, "No changes") || strings.Contains(printed, "refused") {
		t.Fatalf("output = %q, want an unchanged archive reported plainly", printed)
	}
}

func TestConfigImportPreview_RefusalIsACommandFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, &apiv1.Error{Code: "invalid_archive", Message: "checksum mismatch for state.db"})
	})
	_, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("config import --preview of an invalid archive: %v, want the daemon's refusal", err)
	}
}

func TestConfigImport_PreviewAndConfirmAreExclusive(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := runAppCLI(t, sock, "config", "import", "--preview", "--confirm", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "--preview") {
		t.Fatalf("config import --preview --confirm: %v, want a refusal naming --preview", err)
	}
	if called {
		t.Fatal("the daemon was called")
	}
}

func TestConfigImport_StillRequiresConfirmWithoutPreview(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := runAppCLI(t, sock, "config", "import", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("config import without --confirm: %v", err)
	}
	if called {
		t.Fatal("the daemon was called")
	}
}

func testConfigImportReport() *apiv1.ConfigImportReport {
	restored := func(c apiv1.ConfigImportRestoredCategory, added, changed, removed int64) apiv1.ConfigImportRestored {
		return apiv1.ConfigImportRestored{Category: c, Added: added, Changed: changed, Removed: removed}
	}
	return &apiv1.ConfigImportReport{
		Restored: []apiv1.ConfigImportRestored{
			restored(apiv1.ConfigImportRestoredCategoryShares, 2, 1, 0),
			restored(apiv1.ConfigImportRestoredCategoryAccounts, 0, 0, 0),
			restored(apiv1.ConfigImportRestoredCategoryStacks, 0, 3, 1),
			restored(apiv1.ConfigImportRestoredCategoryStackEnv, 0, 0, 0),
		},
		NotRestored: []apiv1.ConfigImportNotRestored{{
			Kind: apiv1.ConfigImportNotRestoredKindStackEnv, Name: "web", Reason: apiv1.ConfigImportNotRestoredReasonNoPassphrase,
			Message: "the .env file of stack web is not restored: no backup passphrase is available to open the archive's secrets",
		}},
		Secrets:          apiv1.ConfigImportSecretsStatusNoPassphrase,
		PreImportArchive: "hoserva-config-2026-09-01.pre-import.tar.zst",
		PreImportSecrets: apiv1.ConfigImportPreImportSecretsConfigured,
	}
}

// serveImport records the multipart fields of an import request and answers
// with the report.
func serveImport(t *testing.T, report *apiv1.ConfigImportReport, fields map[string]string, request *string) string {
	t.Helper()
	return serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		*request = r.Method + " " + r.URL.Path
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading part: %v", err)
				return
			}
			b, _ := io.ReadAll(part)
			fields[part.FormName()] = string(b)
		}
		writeJSON(t, w, http.StatusOK, report)
	})
}

func writePassphraseFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigImport_SendsThePassphraseFromTheFileAndPrintsTheReport(t *testing.T) {
	var request string
	fields := map[string]string{}
	sock := serveImport(t, testConfigImportReport(), fields, &request)

	printed, err := runAppCLI(t, sock, "config", "import", "--confirm", "--passphrase-file", writePassphraseFile(t, "correct horse\n"), writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --confirm --passphrase-file: %v", err)
	}
	if request != "POST /api/v1/config/import" || fields["confirm"] != "true" || fields["passphrase"] != "correct horse" || fields["archive"] != "archive bytes" {
		t.Fatalf("request = %q, fields = %v", request, fields)
	}
	for _, want := range []string{
		"Shares and share permissions", "2 added, 1 changed",
		"App stacks", "3 changed, 1 removed",
		"Not restored", "stack_env", "web", "no backup passphrase is available",
		"hoserva-config-2026-09-01.pre-import.tar.zst",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "Users, groups and API tokens") {
		t.Errorf("output lists a category that restored nothing:\n%s", printed)
	}
}

func TestConfigImport_WithoutAPassphraseFileSendsNoPassphrase(t *testing.T) {
	var request string
	fields := map[string]string{}
	sock := serveImport(t, testConfigImportReport(), fields, &request)
	if _, err := runAppCLI(t, sock, "config", "import", "--confirm", writeArchiveFile(t)); err != nil {
		t.Fatalf("config import --confirm: %v", err)
	}
	if _, sent := fields["passphrase"]; sent {
		t.Fatalf("a passphrase was sent: %v", fields)
	}
}

func TestConfigImport_JSONOutputIsTheReport(t *testing.T) {
	var request string
	sock := serveImport(t, testConfigImportReport(), map[string]string{}, &request)
	printed, err := runAppCLI(t, sock, "--json", "config", "import", "--confirm", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --confirm --json: %v", err)
	}
	var got apiv1.ConfigImportReport
	if err := json.Unmarshal([]byte(printed), &got); err != nil {
		t.Fatalf("output is not the report's JSON: %v\n%s", err, printed)
	}
	if len(got.NotRestored) != 1 || got.NotRestored[0].Name != "web" || got.PreImportArchive == "" || got.PreImportSecrets != apiv1.ConfigImportPreImportSecretsConfigured {
		t.Fatalf("decoded report = %+v", got)
	}
}

func TestConfigImport_SaysWhichPassphraseSealsThePreImportArchivesSecrets(t *testing.T) {
	for secrets, want := range map[apiv1.ConfigImportPreImportSecrets]string{
		apiv1.ConfigImportPreImportSecretsRequest:    "sealed with the passphrase you gave for this import",
		apiv1.ConfigImportPreImportSecretsConfigured: "sealed with the configured backup passphrase",
		apiv1.ConfigImportPreImportSecretsNone:       "no secrets section",
	} {
		t.Run(string(secrets), func(t *testing.T) {
			var request string
			report := testConfigImportReport()
			report.PreImportSecrets = secrets
			sock := serveImport(t, report, map[string]string{}, &request)
			printed, err := runAppCLI(t, sock, "config", "import", "--confirm", writeArchiveFile(t))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(printed, want) {
				t.Fatalf("output does not contain %q:\n%s", want, printed)
			}
		})
	}
}

func TestConfigImport_ReportsAnImportThatRestoredEverything(t *testing.T) {
	var request string
	report := testConfigImportReport()
	report.NotRestored = []apiv1.ConfigImportNotRestored{}
	report.Secrets = apiv1.ConfigImportSecretsStatusOpened
	sock := serveImport(t, report, map[string]string{}, &request)
	printed, err := runAppCLI(t, sock, "config", "import", "--confirm", writeArchiveFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "Everything in the archive was restored") || strings.Contains(printed, "Not restored") {
		t.Fatalf("output = %q", printed)
	}
}

func TestConfigImport_AnEmptyPassphraseFileIsRefusedBeforeTheDaemonIsCalled(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, content := range []string{"", "\n"} {
		_, err := runAppCLI(t, sock, "config", "import", "--confirm", "--passphrase-file", writePassphraseFile(t, content), writeArchiveFile(t))
		if err == nil || !strings.Contains(err.Error(), "passphrase") {
			t.Fatalf("config import with an empty passphrase file %q: %v", content, err)
		}
	}
	_, err := runAppCLI(t, sock, "config", "import", "--confirm", "--passphrase-file", filepath.Join(t.TempDir(), "missing"), writeArchiveFile(t))
	if err == nil {
		t.Fatal("config import with a missing passphrase file: nil")
	}
	if called {
		t.Fatal("the daemon was called")
	}
}

func TestConfigImportPreview_SendsThePassphraseAndPrintsTheSecretsStatus(t *testing.T) {
	var request string
	fields := map[string]string{}
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		request = r.URL.Path
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			fields[part.FormName()] = string(b)
		}
		p := testConfigImportPreview()
		p.Secrets = apiv1.ConfigImportSecrets{Status: apiv1.ConfigImportSecretsStatusPassphraseIncorrect, Stacks: []string{"media", "web"}}
		writeJSON(t, w, http.StatusOK, p)
	})
	printed, err := runAppCLI(t, sock, "config", "import", "--preview", "--passphrase-file", writePassphraseFile(t, "correct horse"), writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview --passphrase-file: %v", err)
	}
	if request != "/api/v1/config/import/preview" || fields["passphrase"] != "correct horse" {
		t.Fatalf("request = %q, fields = %v", request, fields)
	}
	for _, want := range []string{"does not open", "media, web"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}

	opened := func(w http.ResponseWriter, r *http.Request) {
		p := testConfigImportPreview()
		p.Secrets = apiv1.ConfigImportSecrets{Status: apiv1.ConfigImportSecretsStatusOpened, Stacks: []string{}}
		writeJSON(t, w, http.StatusOK, p)
	}
	printed, err = runAppCLI(t, serveAppAPI(t, opened), "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "stack .env files would be restored") {
		t.Errorf("output does not say the secrets restore:\n%s", printed)
	}
}

func testBareMetalPreview() *apiv1.ConfigImportPreview {
	p := testConfigImportPreview()
	p.Blockers = []apiv1.ConfigImportBlocker{}
	p.BareMetal = apiv1.NewOptConfigImportBareMetal(apiv1.ConfigImportBareMetal{
		SchemaUpgrade: true,
		Disks: []apiv1.ConfigImportDisk{
			{Name: "data disk 1 (/mnt/disk1, WWN wwn-d1)", Role: apiv1.ArrayDiskRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1", State: apiv1.ConfigImportDiskStateMatched, Device: apiv1.NewOptString("/dev/sdb")},
			{Name: "parity disk 1 (/mnt/parity1, WWN wwn-p1)", Role: apiv1.ArrayDiskRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity1", State: apiv1.ConfigImportDiskStateAbsent},
			{Name: "data disk 2 (/mnt/disk2, WWN wwn-d2)", Role: apiv1.ArrayDiskRoleData, RoleIndex: 2, Mountpoint: "/mnt/disk2", State: apiv1.ConfigImportDiskStateReplaced, Device: apiv1.NewOptString("/dev/sdc")},
		},
		DiskMapping: apiv1.ConfigImportDiskMapping{Disks: []apiv1.ConfigImportDiskMappingEntry{{Role: apiv1.ArrayDiskRoleData, RoleIndex: 1, Device: "/dev/sdb"}}},
	})
	return p
}

func TestConfigImportPreview_ShowsTheDiskMappingOfABareMetalRestore(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, testBareMetalPreview())
	})
	printed, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview: %v", err)
	}
	for _, want := range []string{
		"restores another installation's archive",
		"upgraded first",
		"data disk 1 (/mnt/disk1, WWN wwn-d1): matched, on /dev/sdb",
		"parity disk 1 (/mnt/parity1, WWN wwn-p1): absent, not mounted",
		"data disk 2 (/mnt/disk2, WWN wwn-d2): replaced (/dev/sdc), not mounted",
		"the replace flow adopts a replacement disk",
		"--confirm --disk-mapping-file <file>",
		`"device": "/dev/sdb"`,
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, `"/dev/sdc"`) && strings.Contains(printed[strings.Index(printed, "{"):], "sdc") {
		t.Errorf("the mapping to confirm names a disk that did not match:\n%s", printed)
	}
}

func TestConfigImport_SendsTheConfirmedDiskMappingFromTheFile(t *testing.T) {
	var request string
	fields := map[string]string{}
	sock := serveImport(t, testConfigImportReport(), fields, &request)
	path := filepath.Join(t.TempDir(), "mapping.json")
	if err := os.WriteFile(path, []byte(`{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runAppCLI(t, sock, "config", "import", "--confirm", "--disk-mapping-file", path, writeArchiveFile(t)); err != nil {
		t.Fatalf("config import --confirm --disk-mapping-file: %v", err)
	}
	var sent apiv1.ConfigImportDiskMapping
	if err := json.Unmarshal([]byte(fields["diskMapping"]), &sent); err != nil {
		t.Fatalf("the diskMapping part = %q: %v", fields["diskMapping"], err)
	}
	if len(sent.Disks) != 1 || sent.Disks[0].Device != "/dev/sdb" || sent.Disks[0].Role != apiv1.ArrayDiskRoleData || sent.Disks[0].RoleIndex != 1 || fields["confirm"] != "true" {
		t.Fatalf("fields = %v, want the confirmed mapping sent with confirm", fields)
	}
}

func TestConfigImport_WithoutAMappingFileSendsNoDiskMapping(t *testing.T) {
	var request string
	fields := map[string]string{}
	sock := serveImport(t, testConfigImportReport(), fields, &request)
	if _, err := runAppCLI(t, sock, "config", "import", "--confirm", writeArchiveFile(t)); err != nil {
		t.Fatal(err)
	}
	if _, sent := fields["diskMapping"]; sent {
		t.Fatalf("a disk mapping was sent that the user never confirmed: %v", fields)
	}
}

func TestConfigImport_ABadDiskMappingFileIsRefusedBeforeTheDaemonIsCalled(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	notJSON := filepath.Join(t.TempDir(), "not-json")
	if err := os.WriteFile(notJSON, []byte("data 1 -> /dev/sdb"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a file that is not a mapping": {"--confirm", "--disk-mapping-file", notJSON},
		"a file that does not exist":   {"--confirm", "--disk-mapping-file", filepath.Join(t.TempDir(), "missing")},
		"with --preview":               {"--preview", "--disk-mapping-file", notJSON},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runAppCLI(t, sock, append([]string{"config", "import"}, append(args, writeArchiveFile(t))...)...)
			if err == nil {
				t.Fatal("the command succeeded")
			}
			if called {
				t.Fatal("the daemon was called")
			}
		})
	}
}

func TestConfigImport_TheReportNamesTheDisksAndSecretsARestoreLeftOut(t *testing.T) {
	var request string
	report := testConfigImportReport()
	report.NotRestored = []apiv1.ConfigImportNotRestored{
		{Kind: apiv1.ConfigImportNotRestoredKindDisk, Name: "parity disk 1 (/mnt/parity1, WWN wwn-p1)", Reason: apiv1.ConfigImportNotRestoredReasonDiskAbsent, Message: "the parity disk 1 is not mounted: no attached disk is it"},
		{Kind: apiv1.ConfigImportNotRestoredKindDatabaseSecret, Name: "acme_config.dns_secret (nas.example.org)", Reason: apiv1.ConfigImportNotRestoredReasonSealedUnderOtherKey, Message: "cleared"},
	}
	sock := serveImport(t, report, map[string]string{}, &request)
	printed, err := runAppCLI(t, sock, "config", "import", "--confirm", writeArchiveFile(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"disk parity disk 1 (/mnt/parity1, WWN wwn-p1) (disk_absent)", "database_secret acme_config.dns_secret (nas.example.org) (sealed_under_other_key)"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}
}
