package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
	var zipPath string
	var unverified bool
	cmd := &cobra.Command{
		Use:   "scan --flash-backup <zip>",
		Short: "Scan an Unraid Flash Backup and print the go / no-go report",
		Long: "Reads the Flash Backup zip in memory, matches Unraid's disks to this machine's, runs the pre-flight checks " +
			"and prints the written report. Nothing is written to a disk or to the zip. An Unraid version or flash layout " +
			"Hoserva has not been verified against is refused unless --unverified-layout is given, and the report says so.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(zipPath)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			st, err := f.Stat()
			if err != nil {
				return err
			}
			// A real Flash Backup is hundreds of MiB; the daemon bounds how long
			// the upload may take, so the client's own 5-minute request timeout
			// is not applied to it.
			uploader, err := newAPIClientWith(0, nil)
			if err != nil {
				return err
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			req := &apiv1.StartMigrationScanReq{File: ht.MultipartFile{Name: filepath.Base(zipPath), File: f, Size: st.Size()}}
			if unverified {
				req.UnverifiedLayout = apiv1.NewOptBool(true)
			}
			j, err := uploader.StartMigrationScan(apiCtx(), req)
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
	_ = cmd.MarkFlagRequired("flash-backup")
	cmd.Flags().BoolVar(&unverified, "unverified-layout", false, "Scan an Unraid version or flash layout Hoserva has not been verified against; the override is recorded in the report")
	return cmd
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
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
