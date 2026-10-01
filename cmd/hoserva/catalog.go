package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// catalogCmd is `hoserva catalog` (doc 01 §3): the templates of the catalog
// installed on the daemon, read through the same operations the web UI uses.
func catalogCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "catalog", Short: "Browse the template catalog (doc 04 §7)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the catalog's templates and whether each is installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			list, err := c.ListCatalog(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(list)
				return nil
			}
			fmt.Print(catalogListSummary(list))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show TEMPLATE-ID",
		Short: "Show a template's details, the privileges it asks for and its Compose file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			t, err := c.GetCatalogTemplate(apiCtx(), apiv1.GetCatalogTemplateParams{ID: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(t)
				return nil
			}
			fmt.Print(catalogTemplateSummary(t))
			return nil
		},
	})
	return cmd
}

func catalogListSummary(list *apiv1.CatalogList) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Catalog serial %d", list.Serial)
	if at, ok := list.GeneratedAt.Get(); ok {
		fmt.Fprintf(&sb, ", built %s", at.UTC().Format("2006-01-02 15:04 UTC"))
	}
	fmt.Fprintf(&sb, ", %d templates.\n", len(list.Templates))
	tw := tabwriter.NewWriter(&sb, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTITLE\tREVISION\tCATEGORIES\tSOURCE\tINSTALLED")
	for _, e := range list.Templates {
		installed := "no"
		if e.Installed {
			installed = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", e.ID, e.Title, e.Revision, strings.Join(e.Categories, ","), e.Source, installed)
	}
	_ = tw.Flush()
	return sb.String()
}

func catalogTemplateSummary(t *apiv1.CatalogTemplate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s), revision %d, from the %s source.\n", t.Title, t.ID, t.Revision, t.Source)
	fmt.Fprintf(&sb, "Categories: %s\nDocumentation: %s\n", strings.Join(t.Categories, ", "), t.Docs)
	if len(t.Privileges) == 0 {
		fmt.Fprintln(&sb, "Privileges: none beyond an ordinary container.")
	} else {
		fmt.Fprintln(&sb, "Privileges this template asks for:")
		for _, p := range t.Privileges {
			detail := ""
			if d := p.Detail.Or(""); d != "" {
				detail = " " + d
			}
			fmt.Fprintf(&sb, "  - %s%s (service %s): %s\n", p.Kind, detail, p.Service, p.Description)
		}
	}
	fmt.Fprintf(&sb, "\ncompose.yaml:\n%s", t.Compose)
	if !strings.HasSuffix(t.Compose, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}
