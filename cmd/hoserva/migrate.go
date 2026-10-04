package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/google/uuid"
	ht "github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Migrate from Unraid (doc 05)"}
	cmd.AddCommand(migrateScanCmd(), migrateStatusCmd(), migrateReportCmd(), migrateTemplatesCmd(), migrateImportCmd(), migrateVerifyCmd(), migrateForgetCmd())
	return cmd
}

func migrateScanCmd() *cobra.Command {
	var zipPath, device string
	var unverified, fullChecksums bool
	cmd := &cobra.Command{
		Use:   "scan (--flash-backup <zip> | --flash-device <device>)",
		Short: "Scan an Unraid Flash Backup or the Unraid USB stick and print the go / no-go report",
		Long: "Reads Unraid's configuration from the Flash Backup zip, in memory, or from the Unraid USB stick attached to this machine " +
			"(--flash-device /dev/sdX, one of the devices `hoserva migrate status` lists), matches Unraid's disks to this machine's, " +
			"runs the pre-flight checks and prints the written report. Nothing is written to a disk, to the zip or to the stick: the stick " +
			"is mounted read-only for the scan and unmounted again, and nothing is copied from it. An Unraid version or flash layout " +
			"Hoserva has not been verified against is refused unless --unverified-layout is given, and the report says so. " +
			"The scan also reads every data disk the capture records, each through a read-only mount after its read-only filesystem " +
			"check: a disk that fails is refused by name and the scan goes on. It lists every file and hashes every file of 1 MiB or " +
			"less plus a deterministic sample of the larger ones (--full-checksums hashes them all, which takes much longer) as the " +
			"baseline the verify phase compares against. It can take hours on a full array; Ctrl-C stops waiting and the scan " +
			"keeps running as a job, which the API's cancelJob operation stops.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("flash-device") && device == "" {
				return fmt.Errorf("--flash-device needs a device, such as /dev/sdb")
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			var j *apiv1.Job
			if cmd.Flags().Changed("flash-device") {
				req := &apiv1.StartMigrationDeviceScanReq{Device: device}
				if unverified {
					req.UnverifiedLayout = apiv1.NewOptBool(true)
				}
				if fullChecksums {
					req.FullChecksums = apiv1.NewOptBool(true)
				}
				if j, err = c.StartMigrationDeviceScan(apiCtx(), req); err != nil {
					return mapAPIErr(err)
				}
			} else if j, err = uploadFlashBackup(zipPath, unverified, fullChecksums); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done, err := waitForJob(ctx, c, j.ID)
			if err != nil {
				return err
			}
			switch done.Status {
			case apiv1.JobStatusSucceeded:
			case apiv1.JobStatusFailed:
				if e, ok := done.Error.Get(); ok && e.Message != "" {
					return fmt.Errorf("migration scan %s failed: %s", done.ID, e.Message)
				}
				return fmt.Errorf("migration scan %s failed", done.ID)
			default:
				return fmt.Errorf("migration scan %s ended %s", done.ID, done.Status)
			}
			if jsonOutput {
				m, err := c.GetMigration(apiCtx())
				if err != nil {
					return mapAPIErr(err)
				}
				emit(m)
				return nil
			}
			return printMigrationReport(c, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&zipPath, "flash-backup", "", "The Unraid Flash Backup zip to scan")
	cmd.Flags().StringVar(&device, "flash-device", "", "The Unraid USB stick to scan, attached to this machine, such as /dev/sdb")
	cmd.MarkFlagsMutuallyExclusive("flash-backup", "flash-device")
	cmd.MarkFlagsOneRequired("flash-backup", "flash-device")
	cmd.Flags().BoolVar(&unverified, "unverified-layout", false, "Scan an Unraid version or flash layout Hoserva has not been verified against; the override is recorded in the report")
	cmd.Flags().BoolVar(&fullChecksums, "full-checksums", false, "Hash every file of every data disk for the verify baseline, not only a sample; it takes much longer")
	return cmd
}

// uploadFlashBackup sends the zip at zipPath to the daemon and returns the scan
// job it queued.
func uploadFlashBackup(zipPath string, unverified, fullChecksums bool) (*apiv1.Job, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// A real Flash Backup is hundreds of MiB; the daemon bounds how long
	// the upload may take, so the client's own 5-minute request timeout
	// is not applied to it.
	uploader, err := newAPIClientWith(0, nil)
	if err != nil {
		return nil, err
	}
	req := &apiv1.StartMigrationScanReq{File: ht.MultipartFile{Name: filepath.Base(zipPath), File: f, Size: st.Size()}}
	if unverified {
		req.UnverifiedLayout = apiv1.NewOptBool(true)
	}
	if fullChecksums {
		req.FullChecksums = apiv1.NewOptBool(true)
	}
	j, err := uploader.StartMigrationScan(apiCtx(), req)
	if err != nil {
		return nil, mapAPIErr(err)
	}
	return j, nil
}

func printMigrationReport(c *apiv1.Client, w io.Writer) error {
	doc, err := c.GetMigrationReport(apiCtx())
	if err != nil {
		return mapAPIErr(err)
	}
	_, err = io.Copy(w, doc.Data)
	return err
}

var migrationPhaseLabels = map[apiv1.MigrationPhase]string{
	apiv1.MigrationPhaseNone:       "no scan yet",
	apiv1.MigrationPhaseScanning:   "scanning",
	apiv1.MigrationPhaseScanFailed: "the last scan did not finish",
	apiv1.MigrationPhaseScanned:    "scanned",
	apiv1.MigrationPhaseImported:   "imported: the data disks are adopted read-only, waiting for the point of no return",

	apiv1.MigrationPhaseVerifying:    "verifying the adopted disks against the scan's baseline",
	apiv1.MigrationPhaseVerifyFailed: "verify failed: the adopted disks differ from the scan's baseline, or the verify did not finish",
	apiv1.MigrationPhaseVerified:     "verified: the adopted disks match the scan's baseline",
}

func migrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the migration session and the verdict of its report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			m, err := c.GetMigration(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(m)
				return nil
			}
			fmt.Printf("Migration: %s\n", migrationPhaseLabels[m.Phase])
			if e, ok := m.ScanError.Get(); ok {
				fmt.Printf("Scan error: %s\n", e)
			}
			if dev, ok := m.SourceDevice.Get(); ok {
				fmt.Printf("Source: the Unraid USB stick at %s\n", dev)
			}
			switch {
			case m.ZipOnly:
				fmt.Println("Flash devices: none; Unraid booted from an internal device, so the Flash Backup zip is the only source")
			case len(m.FlashDevices) == 0:
				fmt.Println("Flash devices: none attached")
			}
			for _, d := range m.FlashDevices {
				fmt.Printf("Flash device: %s (%s)\n", d.Device, strings.TrimSpace(d.Model.Or("")+fmt.Sprintf(" %.1f GiB", float64(d.Size)/(1<<30))))
			}
			if r, ok := m.Report.Get(); ok {
				if v, ok := r.UnraidVersion.Get(); ok {
					fmt.Printf("Unraid version: %s\n", v)
				}
				if r.UnverifiedLayout {
					fmt.Println("Unverified layout: the scan went ahead only because --unverified-layout overrode the refusal")
				}
				fmt.Printf("Verdict: %s\n", r.Verdict)
				counts := map[apiv1.MigrationCheckStatus]int{}
				for _, row := range r.Rows {
					counts[row.Status]++
				}
				fmt.Printf("Findings: %d refuse, %d flag, %d warn\n", counts[apiv1.MigrationCheckStatusRefuse], counts[apiv1.MigrationCheckStatusFlag], counts[apiv1.MigrationCheckStatusWarn])
			}
			switch m.Phase {
			case apiv1.MigrationPhaseImported, apiv1.MigrationPhaseVerifying, apiv1.MigrationPhaseVerifyFailed, apiv1.MigrationPhaseVerified:
				return printSeeded(c)
			}
			return nil
		},
	}
}

func migrateReportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Print the go / no-go report, or save it with -o",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			if out == "" {
				return printMigrationReport(c, os.Stdout)
			}
			doc, err := c.GetMigrationReport(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if err := writeFileAtomic(out, doc.Data); err != nil {
				return err
			}
			if jsonOutput {
				emit(map[string]string{"path": out})
			} else {
				fmt.Println(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "Save the report to this file instead of printing it")
	return cmd
}

func migrateTemplatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "templates [NAME]",
		Short: "Show how the scan's Docker templates convert, or one template's preview",
		Long: "Without a name, lists every template and Compose Manager project of the latest scan with its class and how it " +
			"converted, and the counts the report shows. With a template's file name (my-notes.xml) or a Compose Manager " +
			"project's name, prints that preview: its source, the generated Compose, every warning and the privileges it asks " +
			"for. Nothing is created or run; a preview holds the template's environment, secrets included.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				pv, err := c.GetMigrationTemplate(apiCtx(), apiv1.GetMigrationTemplateParams{Name: args[0]})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(pv)
					return nil
				}
				fmt.Print(migrationPreviewReport(pv))
				return nil
			}
			list, err := c.ListMigrationTemplates(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(list)
				return nil
			}
			printMigrationTemplates(os.Stdout, list)
			return nil
		},
	}
}

func printMigrationTemplates(w io.Writer, list *apiv1.MigrationTemplates) {
	c := list.Counts
	scope := "installed templates"
	if c.AllTemplates {
		scope = "templates (no container list in the capture, so every template is counted)"
	}
	_, _ = fmt.Fprintf(w, "Counted %s: %d convert cleanly, %d with warnings, %d could not be converted\n", scope, c.Clean, c.WithWarnings, c.Failed)
	_, _ = fmt.Fprintf(w, "Template only (previewed, not counted): %d\nCompose Manager projects previewed (not converted): %d\n\n", c.TemplateOnly, c.ComposeProjects)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FILE\tNAME\tCLASS\tCOUNTED\tSTATUS\tWARNINGS")
	for _, t := range list.Templates {
		counted := "no"
		if t.Counted {
			counted = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", t.File, t.Name, t.Class, counted, t.Status, t.WarningCount)
	}
	for _, p := range list.ComposeProjects {
		_, _ = fmt.Fprintf(tw, "%s\tCompose Manager project\t\t\t%s\t\n", p.Name, p.Status)
	}
	_ = tw.Flush()
	for _, t := range list.Templates {
		if e, ok := t.Error.Get(); ok {
			_, _ = fmt.Fprintf(w, "\n%s could not be converted: %s\n", t.File, e)
		}
	}
	for _, p := range list.ComposeProjects {
		if e, ok := p.Error.Get(); ok {
			_, _ = fmt.Fprintf(w, "\n%s: %s\n", p.Name, e)
		}
	}
}

// migrationPreviewReport prints a template's preview as `hoserva app convert`
// prints a conversion, and a Compose Manager project's compose.yaml with the
// privileges it asks for.
func migrationPreviewReport(pv *apiv1.MigrationTemplatePreview) string {
	if e, ok := pv.Error.Get(); ok {
		return fmt.Sprintf("%s could not be previewed: %s\n\n=== Source ===\n%s\n", pv.Name, e, strings.TrimRight(pv.Source, "\n"))
	}
	conv := &apiv1.UnraidConversion{
		Source: pv.Source, Compose: pv.Compose.Or(""), Warnings: pv.Warnings, Privileges: pv.Privileges,
		Clean: pv.Status == apiv1.MigrationTemplateStatusClean,
	}
	if pv.Kind == apiv1.MigrationTemplatePreviewKindTemplate {
		return conversionReport(pv.Name, conv)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== Compose Manager project %s: compose.yaml (not converted, nothing is applied) ===\n%s", pv.Name, pv.Source)
	if !strings.HasSuffix(pv.Source, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("\n=== Privileges ===\n")
	if len(pv.Privileges) == 0 {
		sb.WriteString("none beyond an ordinary container\n")
	}
	for _, p := range pv.Privileges {
		detail := ""
		if d := p.Detail.Or(""); d != "" {
			detail = " " + d
		}
		fmt.Fprintf(&sb, "  - %s%s (service %s): %s\n", p.Kind, detail, p.Service, p.Description)
	}
	return sb.String()
}

func migrateVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Compare the adopted disks with the scan's baseline, before parity is touched",
		Long: "Step 16 of the migration, the last checkpoint where problems are cheap. Walks every adopted data disk through its " +
			"read-only mount and every share through the read-only pool, compares file, symlink and special-file counts, total bytes, " +
			"every file's size and every symlink's target with the scan's baseline, and hashes again exactly the files the baseline " +
			"hashed. A path two disks hold is shown once by the pool, from the first disk, and is listed, never counted as missing or " +
			"extra. Nothing is written to a source disk. It waits for the comparison and prints it, and exits non-zero unless every " +
			"disk and share matches; a verify can be run again. Ctrl-C stops waiting and the job keeps running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			j, err := c.StartMigrationVerify(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done, err := waitForJob(ctx, c, j.ID)
			if err != nil {
				return err
			}
			m, err := c.GetMigration(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			v, haveResult := m.Verify.Get()
			if jsonOutput {
				if haveResult {
					emit(v)
				} else {
					emit(done)
				}
			} else if haveResult {
				printMigrationVerify(os.Stdout, v)
			}
			if done.Status == apiv1.JobStatusSucceeded && haveResult && v.Status == apiv1.MigrationVerifyStatusPassed {
				if !jsonOutput {
					fmt.Println("Verify passed: every adopted disk and share matches the scan's baseline.")
				}
				return nil
			}
			switch {
			case done.Status == apiv1.JobStatusSucceeded:
				return fmt.Errorf("migration verify %s ended without a passing result: do not go on", done.ID)
			case done.Status != apiv1.JobStatusFailed:
				return fmt.Errorf("migration verify %s ended %s: it did not pass, do not go on", done.ID, done.Status)
			}
			if e, ok := done.Error.Get(); ok && e.Message != "" {
				return fmt.Errorf("migration verify %s failed: %s", done.ID, e.Message)
			}
			return fmt.Errorf("migration verify %s failed", done.ID)
		},
	}
}

// printMigrationVerify prints the comparison of every disk and share, and under
// it what differs.
func printMigrationVerify(w io.Writer, v apiv1.MigrationVerify) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SCOPE\tFILES EXPECTED\tFILES FOUND\tBYTES EXPECTED\tBYTES FOUND\tSAMPLE HASHED\tRESULT")
	row := func(kind string, s apiv1.MigrationVerifyScope) {
		name := s.Name
		if name == "" {
			name = "(files in the pool's root)"
		}
		result := "match"
		if !s.Passed {
			result = "DIFFERS"
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%d\t%d\t%d\t%d\t%d\t%s\n", kind, name, s.Expected.Files, s.Found.Files, s.Expected.Bytes, s.Found.Bytes, s.Hashed, result)
	}
	for _, d := range v.Disks {
		row("disk", d)
	}
	for _, sh := range v.Shares {
		row("share", sh)
	}
	_ = tw.Flush()
	if e, ok := v.Error.Get(); ok {
		_, _ = fmt.Fprintf(w, "\nThe verify did not finish: %s\n", e)
	}
	for _, group := range [][]apiv1.MigrationVerifyScope{v.Disks, v.Shares} {
		for _, s := range group {
			if s.Passed {
				continue
			}
			name := s.Name
			if name == "" {
				name = "(pool root)"
			}
			if p, ok := s.Problem.Get(); ok {
				_, _ = fmt.Fprintf(w, "\n%s: %s\n", name, p)
			}
			for _, l := range []struct {
				what string
				list apiv1.MigrationVerifyList
			}{
				{"missing", s.Missing}, {"not in the baseline", s.Extra}, {"size changed", s.SizeChanged},
				{"checksum changed", s.ChecksumChanged}, {"kind or link target changed", s.Changed},
			} {
				if l.list.Total == 0 {
					continue
				}
				_, _ = fmt.Fprintf(w, "\n%s: %d %s\n", name, l.list.Total, l.what)
				for _, p := range l.list.Paths {
					_, _ = fmt.Fprintf(w, "  %s\n", p)
				}
				if int64(len(l.list.Paths)) < l.list.Total {
					_, _ = fmt.Fprintf(w, "  ... and %d more\n", l.list.Total-int64(len(l.list.Paths)))
				}
			}
		}
	}
	if v.Duplicates > 0 {
		_, _ = fmt.Fprintf(w, "\n%d path(s) are on more than one disk and are shown once through the pool, from the first disk; they are not counted as missing or extra:\n", v.Duplicates)
		for _, d := range v.DuplicateSample {
			_, _ = fmt.Fprintf(w, "  %s (%s)\n", d.Path, strings.Join(d.Disks, ", "))
		}
	}
}

func migrateForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget",
		Short: "Delete the migration session, its report and the stored Flash Backup zip",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			if err := c.ForgetMigration(apiCtx()); err != nil {
				return mapAPIErr(err)
			}
			if !jsonOutput {
				fmt.Println("Migration session deleted.")
			}
			return nil
		},
	}
}

// writeFileAtomic writes r to path through a temporary file in the same
// directory, so a failed download never replaces a good file.
func writeFileAtomic(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func migrateImportCmd() *cobra.Command {
	var roles, cachePartitions []string
	var yes bool
	cmd := &cobra.Command{
		Use:   "import [--role <serial>=<role> ...] [--cache-partition <by-id>:<partuuid>] --yes",
		Short: "Adopt the Unraid data disks read-only, without formatting or writing them",
		Long: "Phase C of the migration (doc 05 §4 steps 14-16). Adopts the Unraid data disks into the pool at /mnt/user without " +
			"formatting them and without writing a byte to them: each is mounted read-only by its own device, and the pool over them " +
			"is read-only. The former parity and cache disks are recorded and left untouched; formatting them is the point of no " +
			"return, a later step. The roles are pre-filled from the scan's disk table (`hoserva migrate status`); without the " +
			"capture's disks.ini nothing is pre-filled and every role is yours. --role <serial>=<role> (or wwn:<wwn>=<role>) sets or " +
			"overrides one disk's role: parity, data, cache or ignore. A cache on a spare partition of the boot disk is named with " +
			"--cache-partition. The mapping is printed; check it against the serial table you noted from Unraid, then give --yes. " +
			"While the import is pending, parity, array-write and topology jobs are refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			m, err := c.GetMigration(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			mapping, err := importMapping(m, roles, cachePartitions)
			if err != nil {
				return err
			}
			printImportMapping(os.Stdout, mapping)
			if !yes {
				return fmt.Errorf("migrate import adopts these disks read-only, and records the parity and cache disks as they are to be formatted later; check the mapping above against your serial table and run it again with --yes")
			}
			j, err := c.StartMigrationImport(apiCtx(), &apiv1.MigrationImportRequest{Roles: mapping, Confirm: true})
			if err != nil {
				return mapAPIErr(err)
			}
			ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done, err := waitForJob(ctx, c, j.ID)
			if err != nil {
				return err
			}
			switch done.Status {
			case apiv1.JobStatusSucceeded:
			case apiv1.JobStatusFailed:
				if e, ok := done.Error.Get(); ok && e.Message != "" {
					return fmt.Errorf("migration import %s failed: %s", done.ID, e.Message)
				}
				return fmt.Errorf("migration import %s failed", done.ID)
			default:
				return fmt.Errorf("migration import %s ended %s", done.ID, done.Status)
			}
			if jsonOutput {
				emit(done)
				return nil
			}
			printImportLog(c, j.ID)
			fmt.Println("The data disks are adopted read-only at /mnt/user, with the shares and accounts of the Unraid configuration. Set a password for each account listed above, check the pool, then verify before parity is touched.")
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&roles, "role", nil, "A disk's role as <serial>=<role> or wwn:<wwn>=<role> (parity, data, cache or ignore); repeatable. Overrides the scan's proposal")
	cmd.Flags().StringArrayVar(&cachePartitions, "cache-partition", nil, "A spare partition of the boot disk as the cache, as <by-id name>:<partuuid>")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm the mapping shown (required)")
	return cmd
}

// printImportLog prints what the finished import job says it did, which is
// where each created account is listed with the reminder to set its password
// and each share that was not created with the reason. The import has already
// succeeded, so a log that cannot be read is said, not a failure.
func printImportLog(c *apiv1.Client, id uuid.UUID) {
	log, err := c.GetJobLog(apiCtx(), apiv1.GetJobLogParams{JobId: id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "The import's log could not be read (%v); `hoserva logs --job %s` shows it.\n", err, id)
		return
	}
	zr, err := gzip.NewReader(log.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "The import's log could not be read (%v); `hoserva logs --job %s` shows it.\n", err, id)
		return
	}
	defer func() { _ = zr.Close() }()
	fmt.Println("Import log:")
	if _, err := io.Copy(os.Stdout, zr); err != nil {
		fmt.Fprintf(os.Stderr, "The import's log could not be read to its end: %v\n", err)
	}
	fmt.Println()
}

// printSeeded lists what the import created: each share it seeded with the cache
// mode still to be applied and what could not be mapped exactly, and each
// account that has no password yet.
func printSeeded(c *apiv1.Client) error {
	shares, err := c.ListShares(apiCtx())
	if err != nil {
		return mapAPIErr(err)
	}
	users, err := c.ListUsers(apiCtx())
	if err != nil {
		return mapAPIErr(err)
	}
	for _, sh := range shares.Shares {
		m, ok := sh.Migration.Get()
		if !ok {
			continue
		}
		line := fmt.Sprintf("Share %s: %s", sh.Name, sh.CacheMode)
		if t, ok := m.TargetCacheMode.Get(); ok {
			line += fmt.Sprintf(", cache mode %s once the cache exists", t)
		}
		fmt.Println(line)
		for _, n := range m.Notes {
			fmt.Printf("  %s\n", n)
		}
	}
	for _, u := range users.Users {
		if u.Role == apiv1.UserRoleShareOnly && !u.HasCredential {
			fmt.Printf("Account %s: no password set yet\n", u.Username)
		}
	}
	return nil
}

// importMapping is the disk-role mapping the import sends: the scan's proposals
// for the disks it could match to this machine, then the user's own --role and
// --cache-partition entries over them. A proposal for a disk this machine
// boots from is not sent as a whole-disk cache: only a spare partition of it
// can be one. A disk is never given a role the user did not see in the printed
// table; without a proposal and without --role nothing is sent for it.
func importMapping(m *apiv1.Migration, roles, cachePartitions []string) ([]apiv1.MigrationImportDisk, error) {
	type key struct{ serial, wwn string }
	var order []key
	byKey := map[key]apiv1.MigrationImportRole{}
	set := func(k key, r apiv1.MigrationImportRole) {
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = r
	}
	var rows []key
	if rep, ok := m.Report.Get(); ok {
		if rv, ok := rep.Review.Get(); ok {
			for _, d := range rv.Disks {
				rows = append(rows, key{serial: d.Serial.Or(""), wwn: d.Wwn.Or("")})
				p, ok := d.ProposedRole.Get()
				if !ok || p == apiv1.MigrationProposedRoleIgnore || d.Refused {
					continue
				}
				if p == apiv1.MigrationProposedRoleCache && d.HostBoot.Or(false) {
					continue
				}
				switch {
				case d.Wwn.Or("") != "":
					set(key{wwn: d.Wwn.Value}, apiv1.MigrationImportRole(p))
				case d.Serial.Or("") != "":
					set(key{serial: d.Serial.Value}, apiv1.MigrationImportRole(p))
				}
			}
		}
	}
	// A --role names a disk by serial or WWN; the proposals are keyed by WWN
	// when the disk has one. The override takes the key of the table row it
	// names, so it replaces that disk's proposal instead of adding a second
	// entry for the same disk.
	rowKey := func(k key) key {
		for _, r := range rows {
			if k.wwn != "" && strings.EqualFold(r.wwn, k.wwn) || k.serial != "" && r.serial == k.serial {
				if r.wwn != "" {
					return key{wwn: r.wwn}
				}
				return key{serial: r.serial}
			}
		}
		return k
	}
	for _, spec := range roles {
		id, role, ok := strings.Cut(spec, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("--role %q: want <serial>=<role> or wwn:<wwn>=<role>", spec)
		}
		r := apiv1.MigrationImportRole(role)
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("--role %q: the role is parity, data, cache or ignore", spec)
		}
		if wwn, isWWN := strings.CutPrefix(id, "wwn:"); isWWN {
			set(rowKey(key{wwn: wwn}), r)
		} else {
			set(rowKey(key{serial: id}), r)
		}
	}
	out := make([]apiv1.MigrationImportDisk, 0, len(order)+len(cachePartitions))
	for _, k := range order {
		d := apiv1.MigrationImportDisk{Role: byKey[k]}
		if k.wwn != "" {
			d.Wwn = apiv1.NewOptString(k.wwn)
		} else {
			d.Serial = apiv1.NewOptString(k.serial)
		}
		out = append(out, d)
	}
	for _, spec := range cachePartitions {
		byID, partUUID, ok := strings.Cut(spec, ":")
		if !ok || byID == "" || partUUID == "" {
			return nil, fmt.Errorf("--cache-partition %q: want <by-id name>:<partuuid>", spec)
		}
		out = append(out, apiv1.MigrationImportDisk{Role: apiv1.MigrationImportRoleCache, ById: apiv1.NewOptString(byID), PartUuid: apiv1.NewOptString(partUUID)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("there is no disk-role mapping: the scan proposed no roles (without the capture's disks.ini every role is yours), so give each disk with --role <serial>=<role>")
	}
	return out, nil
}

func printImportMapping(w io.Writer, mapping []apiv1.MigrationImportDisk) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ROLE\tDISK")
	for _, d := range mapping {
		name := "serial " + d.Serial.Or("")
		switch {
		case d.Wwn.Or("") != "":
			name = "WWN " + d.Wwn.Value
		case d.ById.Or("") != "":
			name = "boot-disk partition " + d.ById.Value + " (PARTUUID " + d.PartUuid.Or("") + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", d.Role, name)
	}
	_ = tw.Flush()
}
