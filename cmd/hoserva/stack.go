package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// stackCmd is `hoserva stack` (doc 01 §3, doc 04 §2): reading and editing a
// Compose stack's docker-compose.yml, and starting the stack so an edit
// takes effect.
func stackCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "stack", Short: "Compose stack commands"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the Compose stacks and whether each was manually edited",
			Args:  cobra.NoArgs,
			RunE:  runAPI(func(c *apiv1.Client) (any, error) { return c.ListStacks(apiCtx()) }),
		},
		stackShowCmd(),
		stackEditCmd(),
		stackConfigCmd(),
		stackTemplateUpdateCmd(),
		&cobra.Command{
			Use:   "start NAME",
			Short: "Run docker compose up for a stack as a job, so an edit takes effect; prints the job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runAPI(func(c *apiv1.Client) (any, error) {
					return c.StartStack(apiCtx(), apiv1.StartStackParams{Name: args[0]})
				})(cmd, args)
			},
		},
	)
	return cmd
}

func stackShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Print a stack's docker-compose.yml; with --json the whole stack, never its .env",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			s, err := c.GetStack(apiCtx(), apiv1.GetStackParams{Name: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(s)
				return nil
			}
			if s.ManuallyEdited {
				fmt.Fprintf(os.Stderr, "stack %s was manually edited\n", s.Name)
			}
			fmt.Print(s.Compose.Or(""))
			return nil
		},
	}
}

func stackEditCmd() *cobra.Command {
	var (
		file   string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "edit NAME --file FILE",
		Short: "Replace a stack's docker-compose.yml with FILE after docker compose config accepts it; nothing is restarted",
		Long: "The file is validated on the daemon with `docker compose config` against the stack's own .env " +
			"before anything is stored. A valid file is stored, the stack is marked manually edited so its " +
			"template form never silently overwrites it, and its docker-compose.yml is regenerated. Nothing is " +
			"restarted: run `hoserva stack start NAME` to make the edit take effect. With --dry-run the file is " +
			"only validated.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("stack edit requires --file")
			}
			text, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			params := apiv1.UpdateStackParams{Name: args[0]}
			if dryRun {
				params.DryRun = apiv1.NewOptBool(true)
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			res, err := c.UpdateStack(apiCtx(), &apiv1.UpdateStackRequest{Compose: string(text)}, params)
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
				return nil
			}
			if res.Applied {
				fmt.Printf("Saved the Compose file of stack %s and marked it manually edited. Run `hoserva stack start %s` to apply it.\n", args[0], args[0])
			} else {
				fmt.Printf("The Compose file is valid for stack %s. Nothing was stored (--dry-run).\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "The new docker-compose.yml")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only validate the file; store nothing")
	return cmd
}

// stackConfigCmd is `hoserva stack config NAME`: the inputs of the template a
// stack was installed from, and with --set or --generate a change of them.
func stackConfigCmd() *cobra.Command {
	var (
		sets     []string
		generate []string
	)
	cmd := &cobra.Command{
		Use:   "config NAME [--set NAME=VALUE]... [--generate NAME]...",
		Short: "Show an installed stack's template inputs, or change them with --set and --generate; nothing is restarted",
		Long: "Without --set and --generate, prints the inputs of the template the stack was installed from with " +
			"the value each has now. A secret is only ever reported as set or not set. With them, the new values " +
			"are validated on the daemon with the rules an install uses and, if all hold, written to the stack's " +
			".env; the stack's docker-compose.yml is never rewritten. --generate gives a secret input a newly " +
			"generated value; a secret given neither flag keeps its value. Nothing is restarted: run " +
			"`hoserva stack start NAME` to make the change take effect.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := parseSets(sets)
			if err != nil {
				return err
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			changing := len(values) > 0 || len(generate) > 0
			var cfg *apiv1.StackConfig
			if changing {
				req := &apiv1.UpdateStackConfigRequest{Generate: generate}
				if len(values) > 0 {
					req.Values = apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues(values))
				}
				cfg, err = c.UpdateStackConfig(apiCtx(), req, apiv1.UpdateStackConfigParams{Name: args[0]})
			} else {
				cfg, err = c.GetStackConfig(apiCtx(), apiv1.GetStackConfigParams{Name: args[0]})
			}
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(cfg)
				return nil
			}
			fmt.Print(stackConfigSummary(cfg, changing))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Input value as NAME=VALUE; repeat for each input")
	cmd.Flags().StringArrayVar(&generate, "generate", nil, "Secret input to give a newly generated value; repeat for each input")
	return cmd
}

func stackConfigSummary(cfg *apiv1.StackConfig, changed bool) string {
	var sb strings.Builder
	st := cfg.Stack
	if changed {
		fmt.Fprintf(&sb, "Saved the inputs of stack %s. Run `hoserva stack start %s` to apply them.\n", st.Name, st.Name)
	}
	fmt.Fprintf(&sb, "Stack %s, installed from %s/%s (revision %s).\n", st.Name, safeText(st.Template.Source), safeText(st.Template.ID), safeText(st.Template.Revision))
	if st.ManuallyEdited {
		fmt.Fprintln(&sb, "Its docker-compose.yml was edited by hand; changing the inputs never rewrites it.")
	}
	fmt.Fprintln(&sb, "Inputs:")
	for _, in := range cfg.Inputs {
		switch {
		case in.Kind == apiv1.StackConfigInputKindSecret && in.Set.Or(false):
			fmt.Fprintf(&sb, "  %s: set (the value is never shown)\n", safeText(in.Name))
		case in.Kind == apiv1.StackConfigInputKindSecret:
			fmt.Fprintf(&sb, "  %s: not set\n", safeText(in.Name))
		case in.ReadOnly:
			fmt.Fprintf(&sb, "  %s: %s (read-only; change it in the Compose file)\n", safeText(in.Name), safeText(valueOrNotSet(in.Value.Or(""))))
		default:
			fmt.Fprintf(&sb, "  %s: %s\n", safeText(in.Name), safeText(valueOrNotSet(in.Value.Or(""))))
		}
	}
	return sb.String()
}

func valueOrNotSet(v string) string {
	if v == "" {
		return "not set"
	}
	return v
}
