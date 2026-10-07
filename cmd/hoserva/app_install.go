package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// appInstallCmd is `hoserva app install <template-id>` (doc 01 §3): it
// resolves the template's inputs on the daemon, with the same port-conflict
// and privilege handling the install form gets, and creates the stack.
func appInstallCmd() *cobra.Command {
	var (
		name        string
		sets        []string
		dryRun      bool
		network     string
		restart     string
		cpus        float64
		memoryMiB   int
		extraParams string
	)
	cmd := &cobra.Command{
		Use:   "install TEMPLATE-ID",
		Short: "Install a catalog template as a Compose stack; nothing is started",
		Long: "Resolves the template's inputs, generates its secrets, moves a taken port to the next free one, " +
			"and creates the stack under /var/lib/hoserva/stacks. The privileges the template's Compose content " +
			"asks for are always printed, with every warning about the settings below. A network that does not " +
			"exist is reported with the command that creates it and refuses the install; Hoserva never creates one. " +
			"With --dry-run nothing is created: the same resolution is shown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := parseSets(sets)
			if err != nil {
				return err
			}
			req := &apiv1.TemplateInstallRequest{}
			if name != "" {
				req.Name = apiv1.NewOptString(name)
			}
			if len(values) > 0 {
				req.Values = apiv1.NewOptTemplateInstallRequestValues(values)
			}
			if err := setContainerSettings(cmd, req, network, restart, cpus, memoryMiB, extraParams); err != nil {
				return err
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			if dryRun {
				plan, err := c.PreviewTemplateInstall(apiCtx(), req, apiv1.PreviewTemplateInstallParams{ID: args[0]})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(plan)
					return nil
				}
				fmt.Print(installSummary("Would install", plan, nil))
				return nil
			}
			res, err := c.InstallTemplate(apiCtx(), req, apiv1.InstallTemplateParams{ID: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
				return nil
			}
			fmt.Print(installSummary("Installed", &res.Plan, &res.Stack))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Stack name; defaults to the template id")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Input value as NAME=VALUE; repeat for each input")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what the install would do without creating anything")
	cmd.Flags().StringVar(&network, "network", "", "Network mode: bridge, host or the name of an existing Docker network (a template with one service)")
	cmd.Flags().StringVar(&restart, "restart", "", "Restart policy: no, always, unless-stopped or on-failure")
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "CPU limit of the service, from 0.01 to 1024 (a template with one service)")
	cmd.Flags().IntVar(&memoryMiB, "memory-mib", 0, "Memory limit of the service in MiB, from 6 (a template with one service)")
	cmd.Flags().StringVar(&extraParams, "extra-params", "", "docker run flags to translate into the service, such as \"--cap-add NET_ADMIN\"; never run by a shell (a template with one service)")
	return cmd
}

// setContainerSettings puts the flags that were given into the request. A
// flag given an empty or zero value is refused instead of read as absent, so
// it is never accepted and ignored.
func setContainerSettings(cmd *cobra.Command, req *apiv1.TemplateInstallRequest, network, restart string, cpus float64, memoryMiB int, extraParams string) error {
	flags := cmd.Flags()
	for _, f := range []struct {
		name  string
		empty bool
	}{{"network", network == ""}, {"restart", restart == ""}, {"extra-params", extraParams == ""}, {"cpus", cpus == 0}, {"memory-mib", memoryMiB == 0}} {
		if flags.Changed(f.name) && f.empty {
			return fmt.Errorf("--%s needs a value; leave the flag out to change nothing", f.name)
		}
	}
	if flags.Changed("network") {
		req.NetworkMode = apiv1.NewOptString(network)
	}
	if flags.Changed("restart") {
		policy := apiv1.TemplateInstallRequestRestart(restart)
		switch policy {
		case apiv1.TemplateInstallRequestRestartNo, apiv1.TemplateInstallRequestRestartAlways, apiv1.TemplateInstallRequestRestartUnlessStopped, apiv1.TemplateInstallRequestRestartOnFailure:
		default:
			return fmt.Errorf("--restart %q must be no, always, unless-stopped or on-failure", restart)
		}
		req.Restart = apiv1.NewOptTemplateInstallRequestRestart(policy)
	}
	if flags.Changed("cpus") {
		req.Cpus = apiv1.NewOptFloat64(cpus)
	}
	if flags.Changed("memory-mib") {
		req.MemoryMiB = apiv1.NewOptInt(memoryMiB)
	}
	if flags.Changed("extra-params") {
		req.ExtraParams = apiv1.NewOptString(extraParams)
	}
	return nil
}

// appNetworksCmd is `hoserva app networks`: the networks an install can put a
// container on (--network).
func appNetworksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "networks",
		Short: "List the Docker networks an install can use with --network",
		Long: "Lists every network the Docker Engine holds. Hoserva never creates one: a network an install names " +
			"that is not listed is reported with the `docker network create` command that makes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			res, err := c.ListDockerNetworks(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
				return nil
			}
			if !res.Available {
				return fmt.Errorf("the networks cannot be listed: %s", res.Message.Or("Docker is not reachable"))
			}
			for _, n := range res.Networks {
				fmt.Printf("%s\t%s\n", safeText(n.Name), safeText(n.Driver))
			}
			return nil
		},
	}
}

func parseSets(sets []string) (apiv1.TemplateInstallRequestValues, error) {
	values := apiv1.TemplateInstallRequestValues{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set %q must be NAME=VALUE", s)
		}
		if _, dup := values[k]; dup {
			return nil, fmt.Errorf("--set %s is given twice", k)
		}
		values[k] = v
	}
	return values, nil
}

// installSummary describes what an install did or would do: the stack, each
// input, and every privilege the Compose content asks for.
func installSummary(verb string, plan *apiv1.TemplateInstallPlan, stack *apiv1.Stack) string {
	var sb strings.Builder
	writeInstallSummary(&sb, verb, plan, stack)
	return sb.String()
}

func writeInstallSummary(w *strings.Builder, verb string, plan *apiv1.TemplateInstallPlan, stack *apiv1.Stack) {
	fmt.Fprintf(w, "%s stack %q from %s/%s (revision %s).\n", verb, plan.Name, safeText(plan.Template.Source), safeText(plan.Template.ID), safeText(plan.Template.Revision))
	if stack == nil {
		fmt.Fprintln(w, "Nothing was created.")
	} else {
		fmt.Fprintln(w, "The stack is created but not started.")
	}
	fmt.Fprintln(w, "Inputs:")
	for _, in := range plan.Inputs {
		switch {
		case in.Generated:
			fmt.Fprintf(w, "  %s: generated, written only to the stack's .env\n", safeText(in.Name))
		case in.RequestedValue.Set:
			fmt.Fprintf(w, "  %s: %s (port %s is already in use, so the next free port was used)\n", safeText(in.Name), safeText(in.Value.Or("")), safeText(in.RequestedValue.Or("")))
		case in.Error.Or("") != "":
			fmt.Fprintf(w, "  %s: %s\n", safeText(in.Name), safeText(in.Error.Or("")))
		case in.Value.Or("") == "":
			fmt.Fprintf(w, "  %s: not set\n", safeText(in.Name))
		default:
			fmt.Fprintf(w, "  %s: %s\n", safeText(in.Name), safeText(in.Value.Or("")))
		}
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range plan.Warnings {
			fmt.Fprintf(w, "  - %s: %s\n", safeText(string(warning.Class)), safeText(warning.Message))
			if cmd := warning.Command.Or(""); cmd != "" {
				fmt.Fprintf(w, "      %s\n", safeText(cmd))
			}
		}
	}
	if len(plan.Privileges) == 0 {
		fmt.Fprintln(w, "Privileges: none beyond an ordinary container.")
		return
	}
	fmt.Fprintln(w, "Privileges this template asks for:")
	for _, p := range plan.Privileges {
		fmt.Fprint(w, privilegeLine(p))
	}
}
