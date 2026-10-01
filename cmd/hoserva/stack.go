package main

import (
	"fmt"
	"os"

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
