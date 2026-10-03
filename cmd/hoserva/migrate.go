package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	ht "github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Migrate from Unraid (doc 05)"}
	cmd.AddCommand(migrateScanCmd(), migrateStatusCmd(), migrateReportCmd(), migrateForgetCmd())
	return cmd
}

func migrateScanCmd() *cobra.Command {
	var zipPath, device string
	var unverified bool
	cmd := &cobra.Command{
		Use:   "scan (--flash-backup <zip> | --flash-device <device>)",
		Short: "Scan an Unraid Flash Backup or the Unraid USB stick and print the go / no-go report",
		Long: "Reads Unraid's configuration from the Flash Backup zip, in memory, or from the Unraid USB stick attached to this machine " +
			"(--flash-device /dev/sdX, one of the devices `hoserva migrate status` lists), matches Unraid's disks to this machine's, " +
			"runs the pre-flight checks and prints the written report. Nothing is written to a disk, to the zip or to the stick: the stick " +
			"is mounted read-only for the scan and unmounted again, and nothing is copied from it. An Unraid version or flash layout " +
			"Hoserva has not been verified against is refused unless --unverified-layout is given, and the report says so.",
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
				if j, err = c.StartMigrationDeviceScan(apiCtx(), req); err != nil {
					return mapAPIErr(err)
				}
			} else if j, err = uploadFlashBackup(zipPath, unverified); err != nil {
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
	return cmd
}

// uploadFlashBackup sends the zip at zipPath to the daemon and returns the scan
// job it queued.
func uploadFlashBackup(zipPath string, unverified bool) (*apiv1.Job, error) {
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
