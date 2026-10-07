package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// assertNoTerminalControl fails when out holds a byte or character a
// terminal could act on: a control character other than a line feed or tab,
// or anything that is not valid UTF-8.
func assertNoTerminalControl(t *testing.T, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Errorf("output is not valid UTF-8: %q", out)
	}
	for i, r := range out {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			t.Errorf("output holds the control character %U at byte %d: %q", r, i, out)
			return
		}
	}
}

func TestSafeTextWritesEveryControlCharacterAsAVisibleEscape(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text and spaces", "Jellyfin 10.10 (media)", "Jellyfin 10.10 (media)"},
		{"non-ASCII letters", "Café 日本", "Café 日本"},
		{"escape sequence", "a\x1b[2Kb", `a\x1b[2Kb`},
		{"nul and bell", "\x00\a", `\x00\a`},
		{"line break and tab", "a\nb\tc", `a\nb\tc`},
		{"carriage return", "a\rb", `a\rb`},
		{"delete", "a\x7fb", `a\x7fb`},
		{"first and last C1", "\u0080\u009f", `\u0080\u009f`},
		{"C1 control sequence introducer", "a\u009b[2Kb", `a\u009b[2Kb`},
		{"lone continuation byte", "a\x9bb", `a\x9bb`},
		{"truncated sequence", "a\xe2\x82", `a\xe2\x82`},
		{"overlong encoding of escape", "\xc0\x9b", `\xc0\x9b`},
		{"bidirectional override", "a\u202eb", `a\u202eb`},
		{"line separator", "a\u2028b", `a\u2028b`},
		{"no-break space", "a\u00a0b", `a\u00a0b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeText(tc.in)
			if got != tc.want {
				t.Errorf("safeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertNoTerminalControl(t, got)
		})
	}
}

func TestSafeBlockKeepsOnlyLineFeedsAndTabs(t *testing.T) {
	got := safeBlock("services:\n\tweb:\r\n  a: \x1b[2K\u009b\xff\n")
	want := "services:\n\tweb:\\r\n  a: \\x1b[2K\\u009b\\xff\n"
	if got != want {
		t.Errorf("safeBlock = %q, want %q", got, want)
	}
	assertNoTerminalControl(t, got)
}

func TestSafeTextOfEveryByteValueIsFreeOfControlCharacters(t *testing.T) {
	var all []byte
	for b := 0; b < 256; b++ {
		all = append(all, byte(b))
	}
	for _, s := range []string{string(all), string(bytes.Repeat([]byte{0xe2, 0x80}, 4))} {
		assertNoTerminalControl(t, safeText(s))
		assertNoTerminalControl(t, strings.ReplaceAll(safeBlock(s), "\n", ""))
	}
}

func TestCopyBlockWritesWhatSafeBlockReturnsHoweverTheReadsAreChunked(t *testing.T) {
	var all []byte
	for b := 0; b < 256; b++ {
		all = append(all, byte(b))
	}
	inputs := map[string]string{
		"every byte value":          string(all),
		"multi-byte and controls":   "caf\u00e9 \u65e5\u672c \U0001f600\n\tservices:\r\n  a: \x1b[2K\u009b\u202e\u2028\u00a0\xff",
		"truncated rune at the end": "ok \xe2\x82",
		"empty":                     "",
		"larger than one chunk":     strings.Repeat("a\u65e5\x1b\xe2", 40000),
	}
	readers := map[string]func(string) io.Reader{
		"one read":     func(in string) io.Reader { return strings.NewReader(in) },
		"one byte":     func(in string) io.Reader { return iotest.OneByteReader(strings.NewReader(in)) },
		"half reads":   func(in string) io.Reader { return iotest.HalfReader(strings.NewReader(in)) },
		"data and EOF": func(in string) io.Reader { return iotest.DataErrReader(strings.NewReader(in)) },
	}
	for iname, in := range inputs {
		for rname, mk := range readers {
			t.Run(iname+"/"+rname, func(t *testing.T) {
				var buf bytes.Buffer
				if err := copyBlock(&buf, mk(in)); err != nil {
					t.Fatalf("copyBlock: %v", err)
				}
				if got, want := buf.String(), safeBlock(in); got != want {
					t.Errorf("copyBlock = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestCopyBlockReturnsTheReadErrorAfterTheEscapedPrefix(t *testing.T) {
	boom := errors.New("connection reset")
	in := "a\x1b[2K\u65e5\xe2\x82"
	var buf bytes.Buffer
	err := copyBlock(&buf, io.MultiReader(strings.NewReader(in), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatalf("copyBlock error = %v, want %v", err, boom)
	}
	if got, want := buf.String(), safeBlock(in); got != want {
		t.Errorf("copyBlock wrote %q before the error, want %q", got, want)
	}
}

func TestCopyBlockReturnsTheWriteError(t *testing.T) {
	boom := errors.New("broken pipe")
	if err := copyBlock(failingWriter{boom}, strings.NewReader("abc")); !errors.Is(err, boom) {
		t.Errorf("copyBlock error = %v, want %v", err, boom)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteErrorPrintsControlCharactersEscaped(t *testing.T) {
	var buf bytes.Buffer
	writeError(&buf, errors.New("input NOTE: \x1b[2K\u009b is refused\nsecond line"))
	got := buf.String()
	assertNoTerminalControl(t, got)
	if want := "hoserva: input NOTE: \\x1b[2K\\u009b is refused\nsecond line\n"; got != want {
		t.Errorf("writeError = %q, want %q", got, want)
	}
}

func controlSample() (esc, raw string) {
	return `\x1b[2K\u009b\xff`, "\x1b[2K\u009b\xff"
}

func TestConversionReportPrintsControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	printed := conversionReport("t"+raw+".xml", &apiv1.UnraidConversion{
		Source:   "<Name>" + raw + "</Name>\r\n",
		Compose:  "services:\n  x:\n    image: x" + raw + "\n",
		Warnings: []apiv1.ConversionWarning{{Class: apiv1.ConversionWarningClassNote, Message: "read" + raw, Detail: apiv1.NewOptString("--flag" + raw), Command: apiv1.NewOptString("docker network create" + raw)}},
		Privileges: []apiv1.TemplatePrivilege{
			{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "y", Description: "Runs with full access to the server."},
			{Kind: apiv1.TemplatePrivilegeKindAddedCapabilities, Service: "y" + raw, Detail: apiv1.NewOptString("NET_ADMIN" + raw), Description: "extra"},
		},
	})
	assertNoTerminalControl(t, printed)
	for _, want := range []string{
		"(t" + esc + ".xml)", "<Name>" + esc + "</Name>\\r", "image: x" + esc, "] read" + esc, "    --flag" + esc, "command: docker network create" + esc,
		"  - privileged (service y): Runs with full access to the server.\n",
		"  - added_capabilities NET_ADMIN" + esc + " (service y" + esc + "): extra\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

func TestMigrationPreviewReportPrintsControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	project := migrationPreviewReport(&apiv1.MigrationTemplatePreview{
		Kind: apiv1.MigrationTemplatePreviewKindComposeProject, Name: "stack" + raw, Source: "services:\n  web: {}" + raw + "\n",
		Privileges: []apiv1.TemplatePrivilege{{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "web" + raw, Detail: apiv1.NewOptString("d" + raw), Description: "Runs with full access to the server."}},
	})
	failed := migrationPreviewReport(&apiv1.MigrationTemplatePreview{
		Kind: apiv1.MigrationTemplatePreviewKindTemplate, Name: "t" + raw, Source: "<x>" + raw + "</x>", Error: apiv1.NewOptString("bad" + raw),
	})
	for _, printed := range []string{project, failed} {
		assertNoTerminalControl(t, printed)
	}
	for _, want := range []string{"project stack" + esc + ":", "web: {}" + esc, "  - privileged d" + esc + " (service web" + esc + "): Runs with full access to the server.\n"} {
		if !strings.Contains(project, want) {
			t.Errorf("project preview lacks %q:\n%s", want, project)
		}
	}
	for _, want := range []string{"t" + esc + " could not be previewed: bad" + esc, "<x>" + esc + "</x>"} {
		if !strings.Contains(failed, want) {
			t.Errorf("failed preview lacks %q:\n%s", want, failed)
		}
	}
}

func TestMigrateListingsPrintControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	var tpl bytes.Buffer
	printMigrationTemplates(&tpl, &apiv1.MigrationTemplates{
		Templates:       []apiv1.MigrationTemplateSummary{{Name: "n" + raw, File: "f" + raw + ".xml", Class: apiv1.MigrationTemplateClassAutostart, Status: apiv1.MigrationTemplateStatusFailed, Error: apiv1.NewOptString("broken" + raw)}},
		ComposeProjects: []apiv1.MigrationComposeProjectSummary{{Name: "p" + raw, Status: apiv1.MigrationTemplateStatusFailed, Error: apiv1.NewOptString("refused" + raw)}},
	})
	var ctr bytes.Buffer
	printMigrationContainers(&ctr, &apiv1.MigrationContainers{
		ParityInitialized: true,
		Templates:         []apiv1.MigrationContainerTemplate{{Name: "n" + raw, File: "f" + raw + ".xml", Class: apiv1.MigrationTemplateClassAutostart, Status: apiv1.MigrationTemplateStatusFailed, Error: apiv1.NewOptString("broken" + raw), Stack: apiv1.NewOptString("s" + raw)}},
		ComposeProjects:   []apiv1.MigrationContainerProject{{Name: "p" + raw, Status: apiv1.MigrationTemplateStatusPreviewed}},
		ByHand:            []apiv1.MigrationByHandContainer{{Name: "h" + raw, Image: apiv1.NewOptString("i" + raw)}},
		Stacks:            []apiv1.MigrationContainerStack{{Name: "s" + raw, Source: "src" + raw, Kind: apiv1.MigrationContainerStackKindTemplate, State: apiv1.MigrationContainerStackStateCreated}},
		Awaiting:          apiv1.NewOptString("a" + raw),
	})
	var data bytes.Buffer
	printDataCheck(&data, &apiv1.MigrationContainerCheck{
		Stack: "s" + raw, Running: true,
		Paths: []apiv1.MigrationDataPath{{Container: "c" + raw, Path: "/mnt/user/p" + raw, Destination: "/d" + raw, Status: apiv1.MigrationDataPathStatusMissing, Error: apiv1.NewOptString("e" + raw)}},
	})
	var results bytes.Buffer
	printStackResults(&results, &apiv1.MigrationStacksCreated{Results: []apiv1.MigrationStackResult{{Name: "n" + raw, Stack: "s" + raw, Status: apiv1.MigrationStackResultStatusFailed, Error: apiv1.NewOptError(apiv1.Error{Code: "c", Message: "m" + raw})}}})
	for name, printed := range map[string]string{"templates": tpl.String(), "containers": ctr.String(), "data check": data.String(), "stack results": results.String()} {
		assertNoTerminalControl(t, printed)
		if !strings.Contains(printed, esc) {
			t.Errorf("%s output lacks the escaped text %q:\n%s", name, esc, printed)
		}
	}
}

func TestMigrateVerifyAndParityInitPrintControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	var verify bytes.Buffer
	printMigrationVerify(&verify, apiv1.MigrationVerify{
		Error: apiv1.NewOptString("stopped" + raw),
		Disks: []apiv1.MigrationVerifyScope{{
			Name: "disk" + raw, Problem: apiv1.NewOptString("problem" + raw),
			Missing: apiv1.MigrationVerifyList{Total: 1, Paths: []string{"/mnt/disk1/a" + raw}},
		}},
		Duplicates:      1,
		DuplicateSample: []apiv1.MigrationVerifyDuplicate{{Path: "/b" + raw, Disks: []string{"disk1" + raw}}},
	})
	var parity bytes.Buffer
	printParityInit(&parity, apiv1.MigrationParityInit{
		UnprotectedWindow: "window" + raw, Rollback: []string{"roll" + raw},
		Erases:       []apiv1.MigrationParityErase{{Role: apiv1.MigrationParityEraseRoleParity, Device: "/dev/sd" + raw, Serial: apiv1.NewOptString("SN" + raw)}},
		Confirmation: apiv1.NewOptString("erase" + raw),
	})
	for name, printed := range map[string]string{"verify": verify.String(), "parity init": parity.String()} {
		assertNoTerminalControl(t, printed)
		for _, want := range []string{esc} {
			if !strings.Contains(printed, want) {
				t.Errorf("%s output lacks the escaped text %q:\n%s", name, want, printed)
			}
		}
	}
	for _, want := range []string{"disk" + esc, "problem" + esc, "/mnt/disk1/a" + esc, "/b" + esc + " (disk1" + esc + ")", "stopped" + esc} {
		if !strings.Contains(verify.String(), want) {
			t.Errorf("verify output lacks %q:\n%s", want, verify.String())
		}
	}
	for _, want := range []string{"window" + esc, "Rollback: roll" + esc, "/dev/sd" + esc, "serial SN" + esc, "Confirmation: erase" + esc} {
		if !strings.Contains(parity.String(), want) {
			t.Errorf("parity init output lacks %q:\n%s", want, parity.String())
		}
	}
}

func TestStackSummariesPrintControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	config := stackConfigSummary(&apiv1.StackConfig{
		Stack:  apiv1.Stack{Name: "s", Template: apiv1.StackTemplate{Source: "src" + raw, ID: "id" + raw, Revision: "1"}},
		Inputs: []apiv1.StackConfigInput{{Name: "A", Kind: apiv1.StackConfigInputKindString, Value: apiv1.NewOptString("v" + raw)}, {Name: "B", Kind: apiv1.StackConfigInputKindString, ReadOnly: true, Value: apiv1.NewOptString("w" + raw)}},
	}, false)
	update := stackTemplateUpdateSummary("s", &apiv1.StackTemplateUpdate{
		Status: apiv1.StackTemplateUpdateStatusUpdateAvailable, Source: apiv1.NewOptString("src" + raw), Diff: apiv1.NewOptString("-a\n+b" + raw + "\r\n"),
	})
	for _, printed := range []string{config, update} {
		assertNoTerminalControl(t, printed)
	}
	for _, want := range []string{"src" + esc + "/id" + esc, "  A: v" + esc, "  B: w" + esc} {
		if !strings.Contains(config, want) {
			t.Errorf("stack config lacks %q:\n%s", want, config)
		}
	}
	for _, want := range []string{"source src" + esc, "+b" + esc + "\\r\n"} {
		if !strings.Contains(update, want) {
			t.Errorf("template update lacks %q:\n%s", want, update)
		}
	}
}

func TestCatalogSourceListPrintsControlCharactersEscaped(t *testing.T) {
	esc, raw := controlSample()
	printed := catalogSourceListSummary(&apiv1.CatalogSourceList{Sources: []apiv1.CatalogSource{{ID: "mine" + raw, URL: "https://example.com/" + raw, Kind: apiv1.CatalogSourceKindUserAdded}}})
	assertNoTerminalControl(t, printed)
	for _, want := range []string{"mine" + esc, "https://example.com/" + esc} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}
