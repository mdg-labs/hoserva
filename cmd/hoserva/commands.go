package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	cmd.AddCommand(&cobra.Command{
		Use:   "acknowledge-degraded",
		Short: "Acknowledge a degraded array and start the services waiting on it (doc 02 §1, Q69)",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.AcknowledgeDegradedArray(apiCtx()) }),
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
	cmd.AddCommand(rebalanceCmd())
	return cmd
}

// rebalanceCmd is `hoserva pool rebalance` (doc 09 §3): run directly
// (--confirm with the exact phrase `pool rebalance plan` returned) to
// queue the job, or `pool rebalance plan` first to preview the moves and
// confirmation phrase without changing anything.
func rebalanceCmd() *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use:   "rebalance",
		Short: "Rebalance the pool, moving files to even out fill levels (doc 09 §3)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("pool rebalance requires --confirm with the exact phrase `pool rebalance plan` returned")
			}
			req := &apiv1.StartRebalanceRequest{Confirmation: confirm}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.StartRebalance(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `pool rebalance plan` (required)")

	cmd.AddCommand(&cobra.Command{
		Use:   "plan",
		Short: "Preview a rebalance: the files it would move and the exact confirmation phrase to type",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.PlanRebalance(apiCtx()) }),
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
	cmd.AddCommand(diskAddCmd())
	cmd.AddCommand(diskReplaceCmd())
	cmd.AddCommand(diskUpgradeCmd())
	cmd.AddCommand(diskRemoveCmd())
	return cmd
}

// diskRemoveCmd is diskAddCmd's own shape for `hoserva disk remove`
// (doc 09 §4): evacuates a data disk's files onto the pool's remaining
// disks. It does not remove the disk from the array's own configuration
// or unmount it (doc 09 §4 steps 7-9) — `disk remove finish` does.
func diskRemoveCmd() *cobra.Command {
	var mountpoint, confirm string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Evacuate a data disk's files — the first step of removing it; `disk remove finish` completes it (doc 09 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("disk remove requires --confirm with the exact phrase `disk remove plan` returned")
			}
			req := &apiv1.EvacuateDiskRequest{Mountpoint: mountpoint, Confirmation: confirm}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.EvacuateDisk(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "The data disk slot to evacuate, e.g. /mnt/disk3 (required)")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `disk remove plan` (required)")
	_ = cmd.MarkFlagRequired("mountpoint")

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Preview evacuating a disk: the files it would move and the exact confirmation phrase to type",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.EvacuateDiskPlanRequest{Mountpoint: mountpoint}
			return runAPI(func(c *apiv1.Client) (any, error) {
				res, err := c.PlanDiskEvacuation(apiCtx(), req)
				if err != nil {
					return nil, err
				}
				// planDiskEvacuation's 200 and 400 are both a nil client
				// error (#367): ogen treats a documented non-default 400
				// as a normal sum-type member, not an error, so a
				// refusal must be turned into a command failure here —
				// otherwise it would print and exit 0, indistinguishable
				// from a real plan for any script or chained command.
				if refusal, ok := res.(*apiv1.EvacuationPlanRefusal); ok {
					return nil, fmt.Errorf("%s", refusal.Message)
				}
				return res, nil
			})(cmd, args)
		},
	}
	plan.Flags().StringVar(&mountpoint, "mountpoint", "", "The data disk slot to evacuate, e.g. /mnt/disk3 (required)")
	_ = plan.MarkFlagRequired("mountpoint")
	cmd.AddCommand(plan)
	cmd.AddCommand(diskRemoveFinishCmd())
	cmd.AddCommand(diskRemoveCancelCmd())

	return cmd
}

// diskRemoveFinishCmd is `hoserva disk remove finish` (doc 09 §4 steps
// 7-9): once the disk is evacuated, takes it out of every pool mount and
// out of SnapRAID, then unmounts it. Prints the queued job.
func diskRemoveFinishCmd() *cobra.Command {
	var mountpoint, confirm string
	cmd := &cobra.Command{
		Use:   "finish",
		Short: "Finish removing an evacuated disk: out of the pool and SnapRAID, then unmounted (doc 09 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("disk remove finish requires --confirm with the exact phrase `disk remove plan` returned for this disk")
			}
			req := &apiv1.FinishDiskRemovalRequest{Mountpoint: mountpoint, Confirmation: confirm}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.FinishDiskRemoval(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "The evacuated data disk's slot, e.g. /mnt/disk3 (required)")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `disk remove plan` (required)")
	_ = cmd.MarkFlagRequired("mountpoint")
	return cmd
}

// diskRemoveCancelCmd is `hoserva disk remove cancel` (#361): clears the
// disk's removal state synchronously — no job, no typed confirmation —
// and returns it to normal use. Refused (and the reason printed) unless
// the disk is evacuated, or evacuating with its own evacuation job no
// longer queued, running or interrupted.
func diskRemoveCancelCmd() *cobra.Command {
	var mountpoint string
	cmd := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel a disk's removal and return it to normal use (doc 09 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			req := &apiv1.CancelDiskRemovalRequest{Mountpoint: mountpoint}
			if err := c.CancelDiskRemoval(apiCtx(), req); err != nil {
				return mapAPIErr(err)
			}
			fmt.Printf("disk removal cancelled: %s is back to normal use\n", mountpoint)
			return nil
		},
	}
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "The data disk slot to stop removing, e.g. /mnt/disk3 (required)")
	_ = cmd.MarkFlagRequired("mountpoint")
	return cmd
}

// diskFilesystemFlag applies --filesystem to a setter accepting
// apiv1.OptArrayDiskFilesystem, shared by disk add and disk replace's own
// plan and apply commands — a value the API itself will refuse
// (`ArrayDiskFilesystem`'s own enum) is passed through unchanged rather
// than validated twice.
func diskFilesystemFlag(cmd *cobra.Command, value string, set func(apiv1.OptArrayDiskFilesystem)) {
	if cmd.Flags().Changed("filesystem") {
		set(apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystem(value)))
	}
}

// diskAddCmd is `hoserva disk add`: run directly (--device and --confirm,
// the exact phrase `disk add plan` returned) to queue the job, or
// `disk add plan` first to preview the mountpoint and confirmation phrase
// without changing anything.
func diskAddCmd() *cobra.Command {
	var device, filesystem, confirm string
	var adopt bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a data disk to the running array (doc 02 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("disk add requires --confirm with the exact phrase `disk add plan` returned")
			}
			req := &apiv1.AddDiskRequest{Device: device, Confirmation: confirm}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			if cmd.Flags().Changed("adopt") {
				req.SetAdopt(apiv1.NewOptBool(adopt))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.AddDisk(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "Device to add, e.g. /dev/sdX (required)")
	cmd.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs)")
	cmd.Flags().BoolVar(&adopt, "adopt", false, "Keep the existing filesystem instead of formatting (Q23)")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `disk add plan` (required)")
	_ = cmd.MarkFlagRequired("device")

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Preview adding a disk: mountpoint and the exact confirmation phrase to type",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.AddDiskPlanRequest{Device: device}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			if cmd.Flags().Changed("adopt") {
				req.SetAdopt(apiv1.NewOptBool(adopt))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.PlanDiskAdd(apiCtx(), req) })(cmd, args)
		},
	}
	plan.Flags().StringVar(&device, "device", "", "Device to add, e.g. /dev/sdX (required)")
	plan.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs)")
	plan.Flags().BoolVar(&adopt, "adopt", false, "Keep the existing filesystem instead of formatting (Q23)")
	_ = plan.MarkFlagRequired("device")
	cmd.AddCommand(plan)

	return cmd
}

// diskReplaceCmd is diskAddCmd's own shape for `hoserva disk replace`
// (doc 02 §4 "Replacing a failed disk").
func diskReplaceCmd() *cobra.Command {
	var mountpoint, device, filesystem, confirm string
	var adopt bool
	cmd := &cobra.Command{
		Use:   "replace",
		Short: "Replace a failed data disk (doc 02 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("disk replace requires --confirm with the exact phrase `disk replace plan` returned")
			}
			req := &apiv1.ReplaceDiskRequest{Mountpoint: mountpoint, Device: device, Confirmation: confirm}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			if cmd.Flags().Changed("adopt") {
				req.SetAdopt(apiv1.NewOptBool(adopt))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.ReplaceDisk(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "The existing data disk slot being replaced, e.g. /mnt/disk2 (required)")
	cmd.Flags().StringVar(&device, "device", "", "Replacement device, e.g. /dev/sdX (required)")
	cmd.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs)")
	cmd.Flags().BoolVar(&adopt, "adopt", false, "Keep the existing filesystem instead of formatting (Q23)")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `disk replace plan` (required)")
	_ = cmd.MarkFlagRequired("mountpoint")
	_ = cmd.MarkFlagRequired("device")

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Preview replacing a disk: the SnapRAID fix it will run and the exact confirmation phrase to type",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.ReplaceDiskPlanRequest{Mountpoint: mountpoint, Device: device}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			if cmd.Flags().Changed("adopt") {
				req.SetAdopt(apiv1.NewOptBool(adopt))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.PlanDiskReplace(apiCtx(), req) })(cmd, args)
		},
	}
	plan.Flags().StringVar(&mountpoint, "mountpoint", "", "The existing data disk slot being replaced, e.g. /mnt/disk2 (required)")
	plan.Flags().StringVar(&device, "device", "", "Replacement device, e.g. /dev/sdX (required)")
	plan.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs)")
	plan.Flags().BoolVar(&adopt, "adopt", false, "Keep the existing filesystem instead of formatting (Q23)")
	_ = plan.MarkFlagRequired("mountpoint")
	_ = plan.MarkFlagRequired("device")
	cmd.AddCommand(plan)

	return cmd
}

// diskUpgradeCmd is `hoserva disk upgrade` (doc 02 §4 "Larger data
// disk"/"Larger parity disk", #289): one command for either a data or a
// parity slot — the API resolves which upgrade flow applies from
// --mountpoint's own role. --new-mountpoint is required only for a parity
// slot, and must be the exact value `disk upgrade plan` returned.
func diskUpgradeCmd() *cobra.Command {
	var mountpoint, device, filesystem, newMountpoint, confirm string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade a data or parity disk to a larger one (doc 02 §4)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm == "" {
				return fmt.Errorf("disk upgrade requires --confirm with the exact phrase `disk upgrade plan` returned")
			}
			req := &apiv1.UpgradeDiskRequest{Mountpoint: mountpoint, Device: device, Confirmation: confirm}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			if cmd.Flags().Changed("new-mountpoint") {
				req.SetNewMountpoint(apiv1.NewOptString(newMountpoint))
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.UpgradeDisk(apiCtx(), req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "The existing data or parity slot being upgraded, e.g. /mnt/disk2 or /mnt/parity1 (required)")
	cmd.Flags().StringVar(&device, "device", "", "The new, larger replacement device, e.g. /dev/sdX (required)")
	cmd.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs; a parity disk is always XFS)")
	cmd.Flags().StringVar(&newMountpoint, "new-mountpoint", "", "Parity upgrades only: the exact newMountpoint `disk upgrade plan` returned")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Exact confirmation phrase from `disk upgrade plan` (required)")
	_ = cmd.MarkFlagRequired("mountpoint")
	_ = cmd.MarkFlagRequired("device")

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Preview upgrading a disk: the steps it will run and the exact confirmation phrase to type",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &apiv1.DiskUpgradePlanRequest{Mountpoint: mountpoint, Device: device}
			diskFilesystemFlag(cmd, filesystem, req.SetFilesystem)
			return runAPI(func(c *apiv1.Client) (any, error) { return c.PlanDiskUpgrade(apiCtx(), req) })(cmd, args)
		},
	}
	plan.Flags().StringVar(&mountpoint, "mountpoint", "", "The existing data or parity slot being upgraded, e.g. /mnt/disk2 or /mnt/parity1 (required)")
	plan.Flags().StringVar(&device, "device", "", "The new, larger replacement device, e.g. /dev/sdX (required)")
	plan.Flags().StringVar(&filesystem, "filesystem", "", "xfs, ext4 or btrfs (default xfs; a parity disk is always XFS)")
	_ = plan.MarkFlagRequired("mountpoint")
	_ = plan.MarkFlagRequired("device")
	cmd.AddCommand(plan)

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

// appCmd is `hoserva app` (doc 01 §3): the list, the lifecycle actions on
// a container by its Engine ID or name — managed and unmanaged alike (doc 04
// §2) — its logs and its stats, and installing a catalog template.
func appCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "app", Short: "App and container commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List containers, managed and unmanaged alike (doc 04 §2)",
		RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListApps(apiCtx()) }),
	})
	cmd.AddCommand(
		appActionCmd("start ID", "Start a container and print its state", func(c *apiv1.Client, id string) (any, error) {
			return c.StartApp(apiCtx(), apiv1.StartAppParams{ID: id})
		}),
		appActionCmd("stop ID", "Stop a container and print its state", func(c *apiv1.Client, id string) (any, error) {
			return c.StopApp(apiCtx(), apiv1.StopAppParams{ID: id})
		}),
		appActionCmd("restart ID", "Restart a container and print its state", func(c *apiv1.Client, id string) (any, error) {
			return c.RestartApp(apiCtx(), apiv1.RestartAppParams{ID: id})
		}),
		appActionCmd("recreate ID", "Pull the image again and replace the container, keeping its configuration; prints the job", func(c *apiv1.Client, id string) (any, error) {
			return c.RecreateApp(apiCtx(), apiv1.RecreateAppParams{ID: id})
		}),
		appActionCmd("stats ID", "Show a running container's CPU, memory, network and block I/O use", func(c *apiv1.Client, id string) (any, error) {
			return c.GetAppStats(apiCtx(), apiv1.GetAppStatsParams{ID: id})
		}),
		appRemoveCmd(),
		appLogsCmd(),
		appInstallCmd(),
		appNetworksCmd(),
		appConvertCmd(),
	)
	cmd.AddCommand(appUpdateCmds()...)
	return cmd
}

func appActionCmd(use, short string, call func(*apiv1.Client, string) (any, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(func(c *apiv1.Client) (any, error) { return call(c, args[0]) })(cmd, args)
		},
	}
}

func appRemoveCmd() *cobra.Command {
	var deleteAppdata bool
	cmd := &cobra.Command{
		Use:   "remove ID",
		Short: "Remove a container; its appdata is kept unless --delete-appdata is given",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := apiv1.RemoveAppParams{ID: args[0]}
			if deleteAppdata {
				params.DeleteAppdata = apiv1.NewOptBool(true)
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.RemoveApp(apiCtx(), params) })(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&deleteAppdata, "delete-appdata", false, "Also delete the container's appdata directories")
	return cmd
}

func appLogsCmd() *cobra.Command {
	var tail int32
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Print a container's logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := apiv1.GetAppLogsParams{ID: args[0]}
			if cmd.Flags().Changed("tail") {
				if tail < 0 || tail > 10000 {
					return fmt.Errorf("--tail must be between 0 and 10000")
				}
				params.Tail = apiv1.NewOptInt32(tail)
			}
			if follow {
				if jsonOutput {
					return fmt.Errorf("--follow streams plain text and cannot be combined with --json")
				}
				params.Follow = apiv1.NewOptBool(true)
				return followAppLogs(params)
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			logs, err := c.GetAppLogs(apiCtx(), params)
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				body, err := io.ReadAll(logs.Data)
				if err != nil {
					return err
				}
				emit(string(body))
				return nil
			}
			_, err = io.Copy(os.Stdout, logs.Data)
			return err
		},
	}
	cmd.Flags().Int32Var(&tail, "tail", 200, "How many trailing lines to start from (0-10000)")
	cmd.Flags().BoolVar(&follow, "follow", false, "Keep streaming new lines until interrupted or the container exits")
	return cmd
}

// followAppLogs streams to stdout until the server ends the stream (nil) or
// the user interrupts (nil too — that is how --follow is meant to end). A
// stream that breaks any other way is an error.
func followAppLogs(params apiv1.GetAppLogsParams) error {
	c, err := newStreamingAPIClient(os.Stdout)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := c.GetAppLogs(ctx, params); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return mapAPIErr(err)
	}
	return nil
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
	var allBlocks bool
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
			if allBlocks {
				req.SetAllBlocks(apiv1.NewOptBool(true))
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
	cmd.Flags().BoolVar(&allBlocks, "all-blocks", false, "Scrub blocks of every age, not only those older than 10 days")
	return cmd
}

func fixCmd() *cobra.Command {
	var confirm bool
	var disk int32
	var path string
	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Start a SnapRAID fix",
		Long: "Restores data from parity. Without --path the fix covers the whole array, or the one disk named by --disk, and brings back every file " +
			"changed or deleted since the last sync. With --path it restores only that file and leaves every other change since the last sync as it is.",
		Example: "  hoserva fix --confirm --path /mnt/user/documents/tax.pdf",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("fix requires --confirm")
			}
			if cmd.Flags().Changed("path") {
				if path == "" {
					return fmt.Errorf("--path must name a file under /mnt/user")
				}
				if cmd.Flags().Changed("disk") {
					return fmt.Errorf("--path and --disk cannot be combined")
				}
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
			if cmd.Flags().Changed("path") {
				req.SetPath(apiv1.NewOptString(path))
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
	cmd.Flags().Int32Var(&disk, "disk", 0, "Data disk number N (/mnt/diskN)")
	cmd.Flags().StringVar(&path, "path", "", "Restore only this file, an absolute path under /mnt/user (cannot be combined with --disk)")
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
					if jsonOutput {
						return fmt.Errorf("--follow streams plain text and cannot be combined with --json")
					}
					return followJobLog(id)
				}
				log, err := c.GetJobLog(apiCtx(), apiv1.GetJobLogParams{JobId: id})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					var text bytes.Buffer
					if err := printFinishedJobLog(log.Data, &text); err != nil {
						return err
					}
					emit(text.String())
					return nil
				}
				return printFinishedJobLog(log.Data, os.Stdout)
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

// followJobLog prints a job's log as it grows. The server streams the log's
// gzip file as the job writes it, so the body is decompressed on the fly
// through a pipe. It returns nil when the server ends the stream (the job
// finished) or the user interrupts, and an error when the transport breaks,
// the server refuses, or the stream is not valid gzip. A stream the server
// closed without the gzip trailer (a job whose log was never closed) ends
// cleanly too: everything it carried was printed.
func followJobLog(id uuid.UUID) error {
	pr, pw := io.Pipe()
	printed := make(chan error, 1)
	go func() {
		err := printGzip(pr, os.Stdout)
		pr.CloseWithError(err)
		printed <- err
	}()

	c, err := newStreamingAPIClient(pw)
	if err != nil {
		_ = pw.Close()
		<-printed
		return err
	}
	ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, callErr := c.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id, Follow: apiv1.NewOptBool(true)})
	if callErr != nil {
		pw.CloseWithError(callErr)
	} else {
		_ = pw.Close()
	}
	printErr := <-printed

	if callErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		return mapAPIErr(callErr)
	}
	if printErr != nil && !errors.Is(printErr, io.ErrUnexpectedEOF) {
		return fmt.Errorf("reading the job log: %w", printErr)
	}
	return nil
}

// printGzip decompresses src to dst as bytes arrive. A src that ends before
// its first byte is an empty log, not an error. One that ends inside the
// first header is not gzip, so it is reported as a header error rather than
// as the io.ErrUnexpectedEOF a caller reads as a missing trailer.
func printGzip(src io.Reader, dst io.Writer) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		if err == io.EOF {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: body ends inside the header", gzip.ErrHeader)
		}
		return err
	}
	_, err = io.Copy(dst, gz)
	return err
}

// printFinishedJobLog prints a job log fetched without --follow. The body of a
// job still running has no gzip trailer, as in followJobLog, and what it
// carried is printed.
func printFinishedJobLog(src io.Reader, dst io.Writer) error {
	if err := printGzip(src, dst); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("reading the job log: %w", err)
	}
	return nil
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Config backup"}
	var outPath string
	var confirm, preview bool
	var passphraseFile, diskMappingFile string

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
		Short: "Import a config archive, or preview what importing it would change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if preview && confirm {
				return fmt.Errorf("--preview changes nothing; drop --confirm to preview, or --preview to import")
			}
			if !preview && !confirm {
				return fmt.Errorf("import requires --confirm")
			}
			if preview && cmd.Flags().Changed("disk-mapping-file") {
				return fmt.Errorf("--disk-mapping-file confirms a mapping, which --preview does not import; drop --preview to confirm it")
			}
			var diskMapping apiv1.OptString
			if cmd.Flags().Changed("disk-mapping-file") {
				m, err := readDiskMappingFile(diskMappingFile)
				if err != nil {
					return err
				}
				diskMapping = apiv1.NewOptString(m)
			}
			var passphrase apiv1.OptString
			if cmd.Flags().Changed("passphrase-file") {
				p, err := readPassphraseFile(passphraseFile)
				if err != nil {
					return err
				}
				passphrase = apiv1.NewOptString(p)
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
			if preview {
				out, err := c.PreviewConfigImport(apiCtx(), &apiv1.PreviewConfigImportReq{
					Archive:    http.MultipartFile{Name: args[0], File: f, Size: st.Size()},
					Passphrase: passphrase,
				})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(out)
				} else {
					printConfigImportPreview(out)
				}
				return nil
			}
			report, err := c.ImportConfig(apiCtx(), &apiv1.ImportConfigReq{
				Confirm:     true,
				Archive:     http.MultipartFile{Name: args[0], File: f, Size: st.Size()},
				Passphrase:  passphrase,
				DiskMapping: diskMapping,
			})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(report)
			} else {
				printConfigImportReport(report)
			}
			return nil
		},
	}
	importCmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm import (required unless --preview)")
	importCmd.Flags().BoolVar(&preview, "preview", false, "List what importing the archive would change, without changing anything")
	importCmd.Flags().StringVar(&diskMappingFile, "disk-mapping-file", "", "File holding the disk mapping to confirm for a restore onto a fresh install (the diskMapping of the preview's bareMetal block, from --preview --json)")
	importCmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "File holding the backup passphrase the archive's secrets were sealed under (default: the configured one)")

	cmd.AddCommand(export, importCmd)
	return cmd
}

var configImportCategoryLabels = map[apiv1.ConfigImportGroupCategory]string{
	apiv1.ConfigImportGroupCategoryShares:        "Shares and share permissions",
	apiv1.ConfigImportGroupCategoryAccounts:      "Users, groups and API tokens",
	apiv1.ConfigImportGroupCategorySchedules:     "Schedules",
	apiv1.ConfigImportGroupCategoryNotifications: "Notifications",
	apiv1.ConfigImportGroupCategoryBackup:        "Backup destinations and appdata backup settings",
	apiv1.ConfigImportGroupCategorySystem:        "System settings",
	apiv1.ConfigImportGroupCategoryCustomConfig:  "Custom config files",
	apiv1.ConfigImportGroupCategoryTemplates:     "App templates",
	apiv1.ConfigImportGroupCategoryStacks:        "App stacks",
}

func printConfigImportPreview(p *apiv1.ConfigImportPreview) {
	fmt.Printf("Archive taken %s on %s by Hoserva %s (schema %s; running schema %s)\n",
		p.Archive.Timestamp.Format(time.RFC3339), p.Archive.Host, p.Archive.HoservaVersion, p.Archive.SchemaVersion, p.LiveSchemaVersion)
	for _, b := range p.Blockers {
		fmt.Printf("Import would be refused (%s): %s\n", b.Code, b.Message)
	}
	changes := 0
	for _, g := range p.Groups {
		n := len(g.Added) + len(g.Changed) + len(g.Removed)
		changes += n
		if n == 0 {
			continue
		}
		fmt.Printf("\n%s\n", configImportCategoryLabels[g.Category])
		for _, group := range []struct {
			mark  string
			items []apiv1.ConfigImportChange
		}{{"+", g.Added}, {"~", g.Changed}, {"-", g.Removed}} {
			for _, c := range group.items {
				fmt.Printf("  %s %s\n", group.mark, describeConfigImportChange(c))
			}
		}
	}
	if changes == 0 && len(p.Groups) > 0 {
		fmt.Println("\nNo changes to the configuration.")
	}
	fmt.Printf("\n%s\n", describeConfigImportSecrets(p.Secrets))
	if id, ok := p.Secrets.Identity.Get(); ok {
		fmt.Println(describeConfigImportIdentity(id))
	}
	for _, n := range p.Notes {
		fmt.Printf("\n%s\n", n.Message)
	}
	if bm, ok := p.BareMetal.Get(); ok {
		printConfigImportBareMetal(bm)
	}
}

func printConfigImportBareMetal(bm apiv1.ConfigImportBareMetal) {
	fmt.Println("\nThis installation has no array, so importing restores another installation's archive onto it.")
	if bm.SchemaUpgrade {
		fmt.Println("The archive's database is older than this Hoserva's and is upgraded first.")
	}
	if len(bm.Disks) == 0 {
		fmt.Println("The archive records no array disks.")
	} else {
		fmt.Println("\nThe archive's array disks, against the attached disks:")
	}
	unmatched := false
	for _, d := range bm.Disks {
		switch d.State {
		case apiv1.ConfigImportDiskStateMatched:
			fmt.Printf("  %s: matched, on %s\n", d.Name, d.Device.Or(""))
		default:
			unmatched = true
			line := fmt.Sprintf("  %s: %s", d.Name, d.State)
			if dev, ok := d.Device.Get(); ok {
				line += fmt.Sprintf(" (%s)", dev)
			}
			fmt.Println(line + ", not mounted")
		}
	}
	if unmatched {
		fmt.Println("A disk that is not matched stays a row of the restored array, unmounted, and the array is degraded: nothing mounts, matched disks included, until the degraded array is acknowledged or the replace flow adopts a replacement disk.")
	}
	mapping, err := json.MarshalIndent(bm.DiskMapping, "", "  ")
	if err != nil {
		fmt.Printf("\nThe mapping to confirm could not be printed: %v\n", err)
		return
	}
	fmt.Printf("\nTo restore with these disks, save this mapping to a file and run the import with --confirm --disk-mapping-file <file>:\n%s\n", mapping)
}

// readDiskMappingFile reads the mapping the user confirmed and returns its
// JSON, checked to be a ConfigImportDiskMapping, as importConfig's diskMapping.
func readDiskMappingFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the disk mapping file: %w", err)
	}
	var m apiv1.ConfigImportDiskMapping
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("the disk mapping file %s is not a disk mapping (the diskMapping of a preview's bareMetal block): %w", path, err)
	}
	doc, err := json.Marshal(&m)
	if err != nil {
		return "", err
	}
	return string(doc), nil
}

func describeConfigImportSecrets(s apiv1.ConfigImportSecrets) string {
	var msg string
	switch s.Status {
	case apiv1.ConfigImportSecretsStatusOpened:
		return "Secrets: the passphrase opens the archive's secrets, so its stack .env files would be restored, and a restore onto a fresh install seals its credentials again under this server's key."
	case apiv1.ConfigImportSecretsStatusNone:
		msg = "Secrets: the archive has no secrets section, so its stack .env files would not be restored"
	case apiv1.ConfigImportSecretsStatusNoPassphrase:
		msg = "Secrets: no backup passphrase is available (--passphrase-file gives one), so the archive's stack .env files would not be restored"
	case apiv1.ConfigImportSecretsStatusPassphraseIncorrect:
		msg = "Secrets: the configured backup passphrase does not open the archive's secrets (--passphrase-file gives another), so its stack .env files would not be restored"
	default:
		msg = "Secrets: " + string(s.Status)
	}
	if len(s.Stacks) > 0 {
		msg += ": " + strings.Join(s.Stacks, ", ")
	}
	return msg + "."
}

// describeConfigImportIdentity says what the archive's identity.age means for
// the backup recipient a restore onto a fresh install leaves this box with.
func describeConfigImportIdentity(s apiv1.ConfigImportSecretsStatus) string {
	switch s {
	case apiv1.ConfigImportSecretsStatusOpened:
		return "Backup recipient: the passphrase opens the archive's identity, so a restore onto a fresh install keeps the archive's recipient for future archives."
	case apiv1.ConfigImportSecretsStatusNone:
		return "Backup recipient: the archive has no identity, so a restore onto a fresh install keeps this server's own recipient."
	case apiv1.ConfigImportSecretsStatusNoPassphrase:
		return "Backup recipient: no backup passphrase is available (--passphrase-file gives one), so a restore onto a fresh install keeps this server's own recipient."
	case apiv1.ConfigImportSecretsStatusPassphraseIncorrect:
		return "Backup recipient: the passphrase does not open the archive's identity, so a restore onto a fresh install keeps this server's own recipient."
	default:
		return "Backup recipient: " + string(s)
	}
}

func readPassphraseFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the passphrase file: %w", err)
	}
	p := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if p == "" {
		return "", fmt.Errorf("the passphrase file %s is empty", path)
	}
	return p, nil
}

func printConfigImportReport(r *apiv1.ConfigImportReport) {
	restoredAny := false
	for _, c := range r.Restored {
		if c.Added+c.Changed+c.Removed == 0 {
			continue
		}
		if !restoredAny {
			fmt.Println("Restored:")
			restoredAny = true
		}
		var counts []string
		for _, n := range []struct {
			count int64
			what  string
		}{{c.Added, "added"}, {c.Changed, "changed"}, {c.Removed, "removed"}} {
			if n.count > 0 {
				counts = append(counts, fmt.Sprintf("%d %s", n.count, n.what))
			}
		}
		fmt.Printf("  %s: %s\n", configImportRestoredLabel(c.Category), strings.Join(counts, ", "))
	}
	if !restoredAny {
		fmt.Println("No changes to the configuration.")
	}
	if len(r.NotRestored) == 0 {
		fmt.Println("\nEverything in the archive was restored.")
	} else {
		fmt.Println("\nNot restored:")
		for _, n := range r.NotRestored {
			fmt.Printf("  %s %s (%s): %s\n", n.Kind, n.Name, n.Reason, n.Message)
		}
	}
	if r.PreImportArchive == "" {
		fmt.Println("\nNo pre-import archive was written: no backup destination was written to.")
	} else {
		fmt.Printf("\nThe configuration as it was before the import is in %s.\n", r.PreImportArchive)
		fmt.Println(describePreImportSecrets(r.PreImportSecrets))
	}
}

func describePreImportSecrets(s apiv1.ConfigImportPreImportSecrets) string {
	switch s {
	case apiv1.ConfigImportPreImportSecretsRequest:
		return "Its secrets, with the stack .env files the import replaced, are sealed with the passphrase you gave for this import."
	case apiv1.ConfigImportPreImportSecretsConfigured:
		return "Its secrets are sealed with the configured backup passphrase."
	case apiv1.ConfigImportPreImportSecretsNone:
		return "It has no secrets section."
	default:
		return "Its secrets: " + string(s)
	}
}

func configImportRestoredLabel(c apiv1.ConfigImportRestoredCategory) string {
	if c == apiv1.ConfigImportRestoredCategoryStackEnv {
		return "Stack .env files"
	}
	return configImportCategoryLabels[apiv1.ConfigImportGroupCategory(c)]
}

func describeConfigImportChange(c apiv1.ConfigImportChange) string {
	kind := strings.ReplaceAll(string(c.Kind), "_", " ")
	if c.Name == "" {
		return kind
	}
	return kind + ": " + c.Name
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
	return readSecret("New password: ")
}

// readSecret reads a secret from a terminal without echoing it, or, with
// stdin redirected, from the first 4096 bytes of stdin without their trailing
// newlines.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
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
