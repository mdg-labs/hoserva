package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

// appConvertCmd is `hoserva app convert <unraid-template.xml>` (doc 01 §3,
// doc 04 §5): the daemon converts the template, and everything comes back
// together, the source XML, the generated Compose and every warning, so the
// whole conversion is read before anything is done with it. Nothing is
// created or run.
func appConvertCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "convert UNRAID-TEMPLATE.xml",
		Short: "Convert an Unraid XML template to Compose for review; nothing is created or run",
		Long: "Prints the template, the generated Compose file and every warning. An ExtraParams flag with no " +
			"Compose equivalent, a host path outside the pool and the cache, and a custom network that has to " +
			"exist first are always listed, never dropped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			xml, err := readTemplateFile(args[0])
			if err != nil {
				return err
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			res, err := c.ConvertUnraidTemplate(apiCtx(), &apiv1.UnraidConvertRequest{XML: xml})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
				return nil
			}
			fmt.Print(conversionReport(args[0], res))
			return nil
		},
	}
}

// readTemplateFile reads a template file, refusing one the daemon would
// refuse for its size rather than sending it.
func readTemplateFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, template.MaxUnraidTemplateBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > template.MaxUnraidTemplateBytes {
		return "", fmt.Errorf("%s is larger than %d bytes, which is more than an Unraid template", name, template.MaxUnraidTemplateBytes)
	}
	return string(data), nil
}

func conversionReport(file string, res *apiv1.UnraidConversion) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== Unraid template (%s) ===\n%s\n", safeText(file), safeBlock(strings.TrimRight(res.Source, "\n")))
	fmt.Fprintf(&sb, "\n=== Generated Compose (nothing is applied) ===\n%s", safeBlock(res.Compose))
	if !strings.HasSuffix(res.Compose, "\n") {
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\n=== Warnings (%d) ===\n", len(res.Warnings))
	for _, w := range res.Warnings {
		fmt.Fprintf(&sb, "[%s] %s\n", safeText(string(w.Class)), safeText(w.Message))
		if d := w.Detail.Or(""); d != "" {
			fmt.Fprintf(&sb, "    %s\n", safeText(d))
		}
		if cmd := w.Command.Or(""); cmd != "" {
			fmt.Fprintf(&sb, "    command: %s\n", safeText(cmd))
		}
	}
	sb.WriteString("\n=== Privileges ===\n")
	if len(res.Privileges) == 0 {
		sb.WriteString("none beyond an ordinary container\n")
	}
	for _, p := range res.Privileges {
		sb.WriteString(privilegeLine(p))
	}
	if res.Clean {
		sb.WriteString("\nThe Compose file needs no manual action.\n")
	} else {
		sb.WriteString("\nThe Compose file needs manual review: see the warnings above.\n")
	}
	return sb.String()
}
