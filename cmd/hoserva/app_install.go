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
		name   string
		sets   []string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "install TEMPLATE-ID",
		Short: "Install a catalog template as a Compose stack; nothing is started",
		Long: "Resolves the template's inputs, generates its secrets, moves a taken port to the next free one, " +
			"and creates the stack under /var/lib/hoserva/stacks. The privileges the template's Compose content " +
			"asks for are always printed. With --dry-run nothing is created: the same resolution is shown.",
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
	return cmd
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
	fmt.Fprintf(w, "%s stack %q from %s/%s (revision %s).\n", verb, plan.Name, plan.Template.Source, plan.Template.ID, plan.Template.Revision)
	if stack == nil {
		fmt.Fprintln(w, "Nothing was created.")
	} else {
		fmt.Fprintln(w, "The stack is created but not started.")
	}
	fmt.Fprintln(w, "Inputs:")
	for _, in := range plan.Inputs {
		switch {
		case in.Generated:
			fmt.Fprintf(w, "  %s: generated, written only to the stack's .env\n", in.Name)
		case in.RequestedValue.Set:
			fmt.Fprintf(w, "  %s: %s (port %s is already in use, so the next free port was used)\n", in.Name, in.Value.Or(""), in.RequestedValue.Or(""))
		case in.Value.Or("") == "":
			fmt.Fprintf(w, "  %s: not set\n", in.Name)
		default:
			fmt.Fprintf(w, "  %s: %s\n", in.Name, in.Value.Or(""))
		}
	}
	if len(plan.Privileges) == 0 {
		fmt.Fprintln(w, "Privileges: none beyond an ordinary container.")
		return
	}
	fmt.Fprintln(w, "Privileges this template asks for:")
	for _, p := range plan.Privileges {
		detail := ""
		if d := p.Detail.Or(""); d != "" {
			detail = " " + d
		}
		fmt.Fprintf(w, "  - %s%s (service %s): %s\n", p.Kind, detail, p.Service, p.Description)
	}
}
