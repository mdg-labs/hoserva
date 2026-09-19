package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Overall system health",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.GetStatus(apiCtx()) }),
	}
}

func poolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Short: "Pool commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Per-disk pool breakdown",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.GetPool(apiCtx()) }),
	})
	return cmd
}

func diskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "disk", Short: "Disk commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List disks",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListDisks(apiCtx()) }),
	})
	return cmd
}

func syncCmd() *cobra.Command {
	var dryRun, confirm bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Start a SnapRAID sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			req := &apiv1.StartSyncRequest{}
			req.SetDryRun(apiv1.NewOptBool(dryRun))
			req.SetConfirm(apiv1.NewOptBool(confirm))
			out, err := c.StartSync(apiCtx(), req)
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the diff without syncing")
	cmd.Flags().BoolVar(&confirm, "force", false, "Confirm syncing past the threshold guard")
	return cmd
}

func scrubCmd() *cobra.Command {
	var percent int
	cmd := &cobra.Command{
		Use:   "scrub",
		Short: "Start a SnapRAID scrub",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			req := &apiv1.StartScrubRequest{}
			if cmd.Flags().Changed("percent") {
				req.SetPercent(apiv1.NewOptInt32(int32(percent)))
			}
			out, err := c.StartScrub(apiCtx(), req)
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	cmd.Flags().IntVar(&percent, "percent", 0, "Scrub percentage cap")
	return cmd
}

func fixCmd() *cobra.Command {
	var confirm bool
	var disk int
	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Start a SnapRAID fix",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("fix requires --confirm")
			}
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			req := &apiv1.StartFixRequest{Confirm: true}
			if disk > 0 {
				req.SetDisk(apiv1.NewOptInt32(int32(disk)))
			}
			out, err := c.StartFix(apiCtx(), req)
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm fix (required)")
	cmd.Flags().IntVar(&disk, "disk", 0, "SnapRAID disk index")
	return cmd
}

func jobsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs",
		Short: "List jobs",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListJobs(apiCtx(), apiv1.ListJobsParams{}) }),
	}
}

func logsCmd() *cobra.Command {
	var jobID string
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Job logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			if jobID != "" {
				id, err := uuid.Parse(jobID)
				if err != nil {
					return err
				}
				log, err := c.GetJobLog(apiCtx(), apiv1.GetJobLogParams{JobId: id})
				if err != nil {
					return mapAPIErr(err)
				}
				if _, err := io.Copy(os.Stdout, log.Data); err != nil {
					return err
				}
				return nil
			}
			out, err := c.ListJobs(apiCtx(), apiv1.ListJobsParams{})
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			if follow {
				return fmt.Errorf("--follow requires a job id via --job")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&jobID, "job", "", "Job id")
	cmd.Flags().BoolVar(&follow, "follow", false, "Follow log output (requires --job)")
	return cmd
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Config backup"}
	var outPath string
	var confirm bool

	export := &cobra.Command{
		Use:   "export",
		Short: "Export a config archive",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			archive, err := c.ExportConfig(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if outPath == "" {
				outPath = "hoserva-config.tar.zst"
			}
			f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, archive.Data); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if jsonOutput {
				emit(map[string]string{"path": outPath})
			} else {
				fmt.Println(outPath)
			}
			return nil
		},
	}
	export.Flags().StringVarP(&outPath, "output", "o", "", "Output path")

	importCmd := &cobra.Command{
		Use:   "import [archive]",
		Short: "Import a config archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("import requires --confirm")
			}
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			st, err := f.Stat()
			if err != nil {
				return err
			}
			req := &apiv1.ImportConfigReq{
				Confirm: true,
				Archive: http.MultipartFile{Name: args[0], File: f, Size: st.Size()},
			}
			if err := c.ImportConfig(apiCtx(), req); err != nil {
				return mapAPIErr(err)
			}
			return nil
		},
	}
	importCmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm import (required)")

	cmd.AddCommand(export, importCmd)
	return cmd
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run prerequisite checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			out, err := c.RunDoctor(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(out)
			} else {
				for _, check := range out.Checks {
					fmt.Printf("[%s] %s: %s\n", check.Status, check.Name, check.Message)
				}
			}
			if out.Overall == apiv1.DoctorCheckStatusFail {
				os.Exit(exitDoctor)
			}
			return nil
		},
	}
}

func userCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Account recovery (root only)"}
	var password string

	reset := &cobra.Command{
		Use:   "reset-password [name]",
		Short: "Reset a user's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				return fmt.Errorf("--password is required")
			}
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			return mapAPIErr(c.ResetUserPassword(apiCtx(), &apiv1.ResetUserPasswordRequest{Password: password},
				apiv1.ResetUserPasswordParams{Username: args[0]}))
		},
	}
	reset.Flags().StringVar(&password, "password", "", "New password")

	disable := &cobra.Command{
		Use:   "disable-totp [name]",
		Short: "Disable a user's TOTP",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			return mapAPIErr(c.DisableUserTotp(apiCtx(), apiv1.DisableUserTotpParams{Username: args[0]}))
		},
	}

	unlock := &cobra.Command{
		Use:   "unlock [name]",
		Short: "Clear login lockout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			return mapAPIErr(c.UnlockUser(apiCtx(), apiv1.UnlockUserParams{Username: args[0]}))
		},
	}

	cmd.AddCommand(reset, disable, unlock)
	return cmd
}

func runAPI(fn func(*apiv1.Client) (any, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c, err := newAPIClient(socketPath)
		if err != nil {
			return err
		}
		out, err := fn(c)
		if err != nil {
			return mapAPIErr(err)
		}
		emit(out)
		return nil
	}
}

func mapAPIErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such file") {
		return fmt.Errorf("could not connect to hoservad at %s — is the daemon running?", socketPath)
	}
	return err
}
