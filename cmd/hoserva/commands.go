package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Overall system health",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.GetStatus(apiCtx()) }),
	}
}

func arrayCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "array", Short: "Start or stop the array"}
	var confirmStop bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Enter maintenance mode and unmount the array (Q70)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirmStop {
				return fmt.Errorf("array stop requires --confirm")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.StopArray(apiCtx(), &apiv1.StopArrayRequest{Confirm: true})
			})(cmd, args)
		},
	}
	stop.Flags().BoolVar(&confirmStop, "confirm", false, "Confirm stopping the array (required)")
	cmd.AddCommand(stop)
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Mount the array and leave maintenance mode (Q70)",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.StartArray(apiCtx()) }),
	})
	return cmd
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
	var percent int32
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
				if percent < 1 || percent > 100 {
					return fmt.Errorf("--percent must be between 1 and 100")
				}
				req.SetPercent(apiv1.NewOptInt32(percent))
			}
			out, err := c.StartScrub(apiCtx(), req)
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	cmd.Flags().Int32Var(&percent, "percent", 0, "Scrub percentage cap")
	return cmd
}

func fixCmd() *cobra.Command {
	var confirm bool
	var disk int32
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
			if cmd.Flags().Changed("disk") {
				if disk < 1 {
					return fmt.Errorf("--disk must be a positive SnapRAID disk index")
				}
				req.SetDisk(apiv1.NewOptInt32(disk))
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
	cmd.Flags().Int32Var(&disk, "disk", 0, "SnapRAID disk index")
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
				if follow {
					return fmt.Errorf("--follow is not supported yet")
				}
				log, err := c.GetJobLog(apiCtx(), apiv1.GetJobLogParams{JobId: id})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					body, err := io.ReadAll(log.Data)
					if err != nil {
						return err
					}
					emit(string(body))
					return nil
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
			tmp, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".*.tmp")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			if err := tmp.Chmod(0o600); err != nil {
				_ = tmp.Close()
				_ = os.Remove(tmpPath)
				return err
			}
			if _, err := io.Copy(tmp, archive.Data); err != nil {
				_ = tmp.Close()
				_ = os.Remove(tmpPath)
				return err
			}
			if err := tmp.Close(); err != nil {
				_ = os.Remove(tmpPath)
				return err
			}
			if err := os.Rename(tmpPath, outPath); err != nil {
				_ = os.Remove(tmpPath)
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

	reset := &cobra.Command{
		Use:   "reset-password [name]",
		Short: "Reset a user's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			password, err := readNewPassword()
			if err != nil {
				return err
			}
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			return mapAPIErr(c.ResetUserPassword(apiCtx(), &apiv1.ResetUserPasswordRequest{Password: password},
				apiv1.ResetUserPasswordParams{Username: args[0]}))
		},
	}

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

func updateCmd() *cobra.Command {
	var check, confirm bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update Hoserva from its signed release index (Q67)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient(socketPath)
			if err != nil {
				return err
			}
			if check {
				out, err := c.CheckForUpdate(apiCtx())
				if err != nil {
					return mapAPIErr(err)
				}
				emit(out)
				return nil
			}
			if !confirm {
				return fmt.Errorf("update requires --confirm")
			}
			out, err := c.ApplyUpdate(apiCtx(), &apiv1.ConfirmUpdateRequest{Confirm: true})
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Check the signed release index without installing")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm installing the update (required)")
	return cmd
}

func rollbackCmd() *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Install the previous release and restore its database snapshot (Q67)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("rollback requires --confirm")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.RollbackUpdate(apiCtx(), &apiv1.ConfirmUpdateRequest{Confirm: true})
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm rollback (required)")
	return cmd
}

func rebootCmd() *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "reboot",
		Short: "Wait for storage jobs, shut down the array, and reboot (Q68, Q70)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("reboot requires --confirm")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.RebootHost(apiCtx(), &apiv1.ConfirmUpdateRequest{Confirm: true})
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm reboot (required)")
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

func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "New password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if len(b) == 0 {
			return "", fmt.Errorf("password is required")
		}
		return string(b), nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(b), "\r\n")
	if password == "" {
		return "", fmt.Errorf("password is required")
	}
	return password, nil
}
