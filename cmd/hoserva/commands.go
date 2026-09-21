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
	cmd.AddCommand(diskExternalCmd())
	return cmd
}

func diskExternalCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "external", Short: "Disks outside the array (Q72)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List external disks",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListExternalDisks(apiCtx()) }),
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "mount LABEL",
		Short: "Mount an external disk by filesystem UUID at /mnt/disks/<label>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.MountExternalDisk(apiCtx(), apiv1.MountExternalDiskParams{Label: apiv1.ExternalDiskLabel(args[0])})
			})(cmd, args)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "eject LABEL",
		Short: "Unmount an external disk, then spin it down",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.EjectExternalDisk(apiCtx(), apiv1.EjectExternalDiskParams{Label: apiv1.ExternalDiskLabel(args[0])})
			})(cmd, args)
		},
	})
	var formatConfirm string
	format := &cobra.Command{
		Use:   "format LABEL",
		Short: "Format an external disk after the same typed confirmation as an array disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if formatConfirm == "" {
				return fmt.Errorf("disk external format requires --confirm with the exact ERASE phrase")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.FormatExternalDisk(apiCtx(), &apiv1.FormatExternalDiskRequest{Confirmation: formatConfirm}, apiv1.FormatExternalDiskParams{Label: apiv1.ExternalDiskLabel(args[0])})
			})(cmd, args)
		},
	}
	format.Flags().StringVar(&formatConfirm, "confirm", "", "Exact typed confirmation (ERASE /dev/sdX)")
	cmd.AddCommand(format)
	return cmd
}

func shareCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "share", Short: "Share commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List shares",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListShares(apiCtx()) }),
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get NAME",
		Short: "Get a share",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.GetShare(apiCtx(), apiv1.GetShareParams{Name: apiv1.ShareName(args[0])})
			})(cmd, args)
		},
	})

	var cacheMode, createPolicy, tmSize string
	var smb, guest, readOnly, browseable, recycle, timeMachine bool
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a share",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.CreateShareRequest{Name: apiv1.ShareName(args[0])}
			if cmd.Flags().Changed("cache-mode") {
				req.SetCacheMode(apiv1.NewOptShareCacheMode(apiv1.ShareCacheMode(cacheMode)))
			}
			if cmd.Flags().Changed("create-policy") {
				req.SetCreatePolicy(apiv1.NewOptArrayCreatePolicy(apiv1.ArrayCreatePolicy(createPolicy)))
			}
			if cmd.Flags().Changed("smb") || cmd.Flags().Changed("guest") || cmd.Flags().Changed("read-only") ||
				cmd.Flags().Changed("browseable") || cmd.Flags().Changed("recycle") || cmd.Flags().Changed("time-machine") {
				s := apiv1.ShareSMB{Enabled: smb, Guest: guest, ReadOnly: readOnly, Browseable: browseable, Recycle: recycle, TimeMachine: timeMachine}
				if timeMachine && tmSize != "" {
					s.TimeMachineMaxSize = apiv1.NewOptNilString(tmSize)
				}
				req.SetSmb(apiv1.NewOptShareSMB(s))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.CreateShare(apiCtx(), req) })(cmd, args)
		},
	}
	create.Flags().StringVar(&cacheMode, "cache-mode", "cache-then-move", "cache-then-move, cache-only, or array-only")
	create.Flags().StringVar(&createPolicy, "create-policy", "mspmfs", "mergerfs create policy")
	create.Flags().BoolVar(&smb, "smb", true, "Export over SMB")
	create.Flags().BoolVar(&guest, "guest", false, "Allow guest access")
	create.Flags().BoolVar(&readOnly, "read-only", false, "Read-only")
	create.Flags().BoolVar(&browseable, "browseable", true, "Browseable")
	create.Flags().BoolVar(&recycle, "recycle", false, "Recycle bin")
	create.Flags().BoolVar(&timeMachine, "time-machine", false, "Time Machine")
	create.Flags().StringVar(&tmSize, "time-machine-max-size", "", "Time Machine max size (Q73), e.g. 500G")
	cmd.AddCommand(create)

	var confirmDelete bool
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "Delete a share definition (files stay on disk)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirmDelete {
				return fmt.Errorf("share rm requires --confirm")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return nil, c.DeleteShare(apiCtx(), &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareParams{Name: apiv1.ShareName(args[0])})
			})(cmd, args)
		},
	}
	rm.Flags().BoolVar(&confirmDelete, "confirm", false, "Confirm removing the share definition (required)")
	cmd.AddCommand(rm)

	var confirmData string
	rmData := &cobra.Command{
		Use:   "rm-data NAME",
		Short: "Delete a share's files (definition stays)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirmData != args[0] {
				return fmt.Errorf("share rm-data requires --confirm equal to the share name")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return nil, c.DeleteShareData(apiCtx(), &apiv1.DeleteShareDataRequest{Confirmation: args[0]}, apiv1.DeleteShareDataParams{Name: apiv1.ShareName(args[0])})
			})(cmd, args)
		},
	}
	rmData.Flags().StringVar(&confirmData, "confirm", "", "Type the share name to confirm deleting its files")
	cmd.AddCommand(rmData)

	cmd.AddCommand(&cobra.Command{
		Use:   "browse NAME [PATH]",
		Short: "List a share directory (may wake disks)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := apiv1.BrowseShareParams{Name: apiv1.ShareName(args[0])}
			if len(args) == 2 {
				params.Path = apiv1.NewOptString(args[1])
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.BrowseShare(apiCtx(), params) })(cmd, args)
		},
	})
	return cmd
}

func networkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Host network settings (Q75)",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.GetNetworkSettings(apiCtx()) }),
	}
	var iface, method, address, gateway string
	var prefix int
	var dns []string
	var dhcp bool
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Apply addressing with a 60-second confirm-or-revert",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.ApplyNetworkSettingsRequest{}
			if iface == "" {
				return fmt.Errorf("network apply requires --interface")
			}
			req.SetInterface(apiv1.NewOptString(iface))
			if dhcp {
				req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
			} else if method != "" {
				req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethod(method)))
			} else {
				req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodStatic))
			}
			if address != "" {
				req.SetAddress(apiv1.NewOptString(address))
			}
			if cmd.Flags().Changed("prefix") {
				req.SetPrefix(apiv1.NewOptInt(prefix))
			}
			if cmd.Flags().Changed("gateway") {
				req.SetGateway(apiv1.NewOptString(gateway))
			}
			if cmd.Flags().Changed("dns") {
				req.DNS = dns
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.ApplyNetworkSettings(apiCtx(), req) })(cmd, args)
		},
	}
	apply.Flags().StringVar(&iface, "interface", "", "Interface to reconfigure")
	apply.Flags().BoolVar(&dhcp, "dhcp", false, "Use DHCP")
	apply.Flags().StringVar(&method, "method", "", "dhcp or static")
	apply.Flags().StringVar(&address, "address", "", "Static address without prefix")
	apply.Flags().IntVar(&prefix, "prefix", 24, "Prefix length for a static address")
	apply.Flags().StringVar(&gateway, "gateway", "", "Default gateway")
	apply.Flags().StringSliceVar(&dns, "dns", nil, "DNS nameservers")
	cmd.AddCommand(apply)
	cmd.AddCommand(&cobra.Command{
		Use:   "confirm",
		Short: "Keep the pending network configuration",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ConfirmNetworkSettings(apiCtx()) }),
	})
	return cmd
}

func syncCmd() *cobra.Command {
	var dryRun, confirm bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Start a SnapRAID sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
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
			c, err := newAPIClient()
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
			c, err := newAPIClient()
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

func moverCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mover", Short: "Mover commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Start a manual mover run (doc 09 §2)",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.StartMover(apiCtx()) }),
	})
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
			c, err := newAPIClient()
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
			c, err := newAPIClient()
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
			c, err := newAPIClient()
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
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run prerequisite checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
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
	cmd.AddCommand(applyHostConfigCmd())
	return cmd
}

func applyHostConfigCmd() *cobra.Command {
	var leaveAll bool
	var samba, nfs, fstab, dockerContainers, dockerImages string
	cmd := &cobra.Command{
		Use:   "apply-host-config",
		Short: "Import or leave unmanaged existing host configuration (Q76)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			choices := map[apiv1.HostConfigID]apiv1.HostConfigDecision{}
			if leaveAll {
				report, err := c.RunDoctor(apiCtx())
				if err != nil {
					return mapAPIErr(err)
				}
				for _, check := range report.Checks {
					kind, ok := hostConfigIDFromDoctor(check.ID)
					if ok {
						choices[kind] = apiv1.HostConfigDecisionLeave
					}
				}
			}
			setChoice := func(id apiv1.HostConfigID, raw string) error {
				if raw == "" {
					return nil
				}
				switch raw {
				case "import":
					choices[id] = apiv1.HostConfigDecisionImport
				case "leave":
					choices[id] = apiv1.HostConfigDecisionLeave
				default:
					return fmt.Errorf("%s must be import or leave", id)
				}
				return nil
			}
			if err := setChoice(apiv1.HostConfigIDHostSamba, samba); err != nil {
				return err
			}
			if err := setChoice(apiv1.HostConfigIDHostNfs, nfs); err != nil {
				return err
			}
			if err := setChoice(apiv1.HostConfigIDHostFstab, fstab); err != nil {
				return err
			}
			if err := setChoice(apiv1.HostConfigIDHostDockerContainers, dockerContainers); err != nil {
				return err
			}
			if err := setChoice(apiv1.HostConfigIDHostDockerImages, dockerImages); err != nil {
				return err
			}
			files := make([]apiv1.HostConfigChoice, 0, len(choices))
			for id, decision := range choices {
				files = append(files, apiv1.HostConfigChoice{ID: id, Decision: decision})
			}
			out, err := c.ApplyHostConfig(apiCtx(), &apiv1.ApplyHostConfigRequest{Files: files})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(out)
				return nil
			}
			for _, f := range out.Files {
				fmt.Printf("%s: %s\n", f.ID, f.Decision)
			}
			fmt.Printf("docker data-root: %s\n", out.DockerDataRoot)
			return nil
		},
	}
	cmd.Flags().BoolVar(&leaveAll, "leave-all", false, "Leave every detected host file unmanaged")
	cmd.Flags().StringVar(&samba, "samba", "", "import or leave")
	cmd.Flags().StringVar(&nfs, "nfs", "", "import or leave")
	cmd.Flags().StringVar(&fstab, "fstab", "", "import or leave")
	cmd.Flags().StringVar(&dockerContainers, "docker-containers", "", "import or leave")
	cmd.Flags().StringVar(&dockerImages, "docker-images", "", "import or leave")
	return cmd
}

func hostConfigIDFromDoctor(id string) (apiv1.HostConfigID, bool) {
	switch {
	case id == string(apiv1.HostConfigIDHostSamba) || strings.HasPrefix(id, "host_samba_"):
		return apiv1.HostConfigIDHostSamba, true
	case id == string(apiv1.HostConfigIDHostNfs) || strings.HasPrefix(id, "host_nfs_"):
		return apiv1.HostConfigIDHostNfs, true
	case id == string(apiv1.HostConfigIDHostFstab) || strings.HasPrefix(id, "host_fstab_"):
		return apiv1.HostConfigIDHostFstab, true
	case id == string(apiv1.HostConfigIDHostDockerContainers) || strings.HasPrefix(id, "host_docker_containers_"):
		return apiv1.HostConfigIDHostDockerContainers, true
	case id == string(apiv1.HostConfigIDHostDockerImages) || strings.HasPrefix(id, "host_docker_images_"):
		return apiv1.HostConfigIDHostDockerImages, true
	default:
		return "", false
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
			c, err := newAPIClient()
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
			c, err := newAPIClient()
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
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			return mapAPIErr(c.UnlockUser(apiCtx(), apiv1.UnlockUserParams{Username: args[0]}))
		},
	}

	cmd.AddCommand(reset, disable, unlock)
	return cmd
}

// tokenCmd manages personal API tokens (Q43, #50) — scripting and remote
// CLI credentials, distinct from userCmd's root-only account recovery
// above.
func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Personal API tokens (Q43)"}

	var role string
	create := &cobra.Command{
		Use:   "create [username] [name]",
		Short: "Create a personal API token, printed once",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			out, err := c.CreateApiToken(apiCtx(),
				&apiv1.CreateApiTokenRequest{Name: args[1], Role: apiv1.ApiTokenRole(role)},
				apiv1.CreateApiTokenParams{Username: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			emit(out)
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", "viewer", "Token scope: admin or viewer (Q43)")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List every account's personal API tokens",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListApiTokens(apiCtx()) }),
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "revoke [id]",
		Short: "Revoke a personal API token immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			return mapAPIErr(c.RevokeApiToken(apiCtx(), apiv1.RevokeApiTokenParams{TokenId: args[0]}))
		},
	})

	return cmd
}

func updateCmd() *cobra.Command {
	var check, confirm bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update Hoserva from its signed release index (Q67)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
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
		c, err := newAPIClient()
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
		if remoteHost != "" {
			return fmt.Errorf("could not connect to hoservad at %s:%d — is the daemon running and reachable?", remoteHost, remotePort)
		}
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
