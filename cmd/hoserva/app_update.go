package main

import (
	"fmt"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// appUpdateCmds are the container update commands under `hoserva app` (doc
// 04 §6): what the update check found, updating one container or several,
// reverting, the bulk-update opt-out, the history of updates that can be
// reverted and how long a previous image is kept. Each calls one API
// operation; updating and reverting queue a job and print it.
func appUpdateCmds() []*cobra.Command {
	return []*cobra.Command{
		{
			Use:   "updates",
			Short: "Show what the daily update check found for each container",
			RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListAppUpdates(apiCtx()) }),
		},
		appActionCmd("update ID", "Update a container: snapshot its appdata when it is on the cache disk, pull the image again and keep the previous image for a revert; prints the job", func(c *apiv1.Client, id string) (any, error) {
			return c.UpdateApp(apiCtx(), apiv1.UpdateAppParams{ID: id})
		}),
		appUpdateAllCmd(),
		appActionCmd("revert ID", "Put a container back on the image it ran before its latest update and restore the appdata snapshot taken before it; prints the job", func(c *apiv1.Client, id string) (any, error) {
			return c.RevertApp(apiCtx(), apiv1.RevertAppParams{ID: id})
		}),
		{
			Use:   "update-history",
			Short: "List the container updates whose previous image is kept, and whether each can be reverted",
			RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListAppUpdateHistory(apiCtx()) }),
		},
		appUpdatePolicyCmd(),
		appUpdateSettingsCmd(),
		appRegistryCredentialsCmd(),
		appRegistryCredentialSetCmd(),
		appRegistryCredentialDeleteCmd(),
	}
}

func appUpdateAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-all [ID...]",
		Short: "Update every container with an update available that has not opted out of bulk updates, or the named ones",
		Long: "With no names, queues one job that updates every container the last update check found a newer image for, " +
			"except those excluded with `hoserva app update-policy ID --exclude`; the excluded ones are listed as skipped. " +
			"Naming containers updates exactly those, whether or not they opted out. " +
			"One container's failure does not stop the others; the job fails, naming each, once they have all been tried.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var req apiv1.OptStartAppUpdatesRequest
			if len(args) > 0 {
				req = apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: args})
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.StartAppUpdates(apiCtx(), req) })(cmd, args)
		},
	}
}

func appUpdatePolicyCmd() *cobra.Command {
	var include, exclude bool
	cmd := &cobra.Command{
		Use:   "update-policy ID",
		Short: "Include a container in, or exclude it from, bulk updates",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if include == exclude {
				return fmt.Errorf("give exactly one of --include and --exclude")
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.SetAppUpdatePolicy(apiCtx(), &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: exclude}, apiv1.SetAppUpdatePolicyParams{ID: args[0]})
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&include, "include", false, "Let bulk updates update this container")
	cmd.Flags().BoolVar(&exclude, "exclude", false, "Make bulk updates skip this container; updating it by name still works")
	return cmd
}

func appUpdateSettingsCmd() *cobra.Command {
	var keepDays int
	cmd := &cobra.Command{
		Use:   "update-settings",
		Short: "Show, or with --keep-days set, how long the previous image is kept for a revert",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("keep-days") {
				return runAPI(func(c *apiv1.Client) (any, error) { return c.GetAppSettings(apiCtx()) })(cmd, args)
			}
			return runAPI(func(c *apiv1.Client) (any, error) {
				return c.UpdateAppSettings(apiCtx(), &apiv1.AppSettings{ImageKeepDays: keepDays})
			})(cmd, args)
		},
	}
	cmd.Flags().IntVar(&keepDays, "keep-days", 0, "Days to keep the image a container ran before an update (1 to 365); applies to later updates")
	return cmd
}
