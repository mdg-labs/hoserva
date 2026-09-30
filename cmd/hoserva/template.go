package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mdg-labs/hoserva/internal/template"
)

// templateCmd holds the catalog author's tools. They work on files and never
// talk to hoservad, so a catalog repository's CI can run them from
// `go run github.com/mdg-labs/hoserva/cmd/hoserva@<version> template lint`.
func templateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "template", Short: "Tools for template authors"}
	cmd.AddCommand(templateLintCmd())
	return cmd
}

type lintFinding struct {
	File    string   `json:"file"`
	Line    int      `json:"line,omitempty"`
	Path    []string `json:"path,omitempty"`
	Message string   `json:"message"`
}

func templateLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint <dir>",
		Short: "Check every <id>/compose.yaml in a catalog checkout (doc 04 §7)",
		Long: "Validates the x-hoserva block of every <id>/compose.yaml under <dir> against its schema version " +
			"and the template rules beyond the schema. Exits non-zero when anything is found.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			found, err := template.Lint(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				out := make([]lintFinding, len(found))
				for i, f := range found {
					out[i] = lintFinding{File: f.File, Line: f.Line, Path: f.Path, Message: f.Message}
				}
				emit(out)
			} else {
				for _, f := range found {
					fmt.Println(f)
				}
			}
			if len(found) > 0 {
				return fmt.Errorf("%d problem(s) found", len(found))
			}
			if !jsonOutput {
				fmt.Println("no problems found")
			}
			return nil
		},
	}
}
