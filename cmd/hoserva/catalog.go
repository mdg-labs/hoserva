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
		Use:   "refresh",
		Short: "Check now for a newer signed catalog and install it",
		Long: "Runs one conditional request for the latest signed catalog (doc 04 §7), even when automatic refresh is off. " +
			"An unchanged catalog downloads nothing. A catalog whose signature or serial does not check out is refused and the installed one is kept. " +
			"Exits non-zero when the check failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			res, err := c.RefreshCatalog(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
			} else if res.Outcome != apiv1.CatalogCheckOutcomeFailed {
				fmt.Print(catalogRefreshSummary(res))
			}
			if res.Outcome == apiv1.CatalogCheckOutcomeFailed {
				return fmt.Errorf("the catalog check failed (%s): %s", res.Reason.Or(""), res.Message.Or(""))
			}
			return nil
		},
	})
	cmd.AddCommand(catalogSettingsCmd())
	cmd.AddCommand(catalogSourceCmd())
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

func catalogSettingsCmd() *cobra.Command {
	var (
		interval    string
		checkOnOpen bool
	)
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show, or with --interval and --check-on-open set, how the catalog checks for updates by itself",
		Long: "Without flags, shows the background check interval (off, 1h, 6h, 12h or 24h) and whether opening the catalog starts a check. " +
			"A flag given sets only that setting. With the interval off and check-on-open off, nothing reaches the catalog host except `hoserva catalog refresh`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			intervalSet := cmd.Flags().Changed("interval")
			openSet := cmd.Flags().Changed("check-on-open")
			if !intervalSet && !openSet {
				return runAPI(func(c *apiv1.Client) (any, error) { return c.GetCatalogSettings(apiCtx()) })(cmd, args)
			}
			var req apiv1.CatalogSettingsUpdate
			if intervalSet {
				req.RefreshInterval = apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshInterval(interval))
			}
			if openSet {
				req.CheckOnOpen = apiv1.NewOptBool(checkOnOpen)
			}
			return runAPI(func(c *apiv1.Client) (any, error) { return c.UpdateCatalogSettings(apiCtx(), &req) })(cmd, args)
		},
	}
	cmd.Flags().StringVar(&interval, "interval", "", "Background check interval: off, 1h, 6h, 12h or 24h")
	cmd.Flags().BoolVar(&checkOnOpen, "check-on-open", true, "Start a check when the catalog is opened and the last one is older than 15 minutes (--check-on-open=false to turn off)")
	return cmd
}

func catalogRefreshSummary(res *apiv1.CatalogRefresh) string {
	at := res.CheckedAt.UTC().Format("2006-01-02 15:04 UTC")
	if res.Outcome == apiv1.CatalogCheckOutcomeUpdated {
		return fmt.Sprintf("Checked %s: the catalog was updated, %d new and %d updated templates.\n", at, res.NewTemplates.Or(0), res.UpdatedTemplates.Or(0))
	}
	return fmt.Sprintf("Checked %s: the catalog is already up to date.\n", at)
}

func catalogListSummary(list *apiv1.CatalogList) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Catalog serial %d", list.Serial)
	if at, ok := list.GeneratedAt.Get(); ok {
		fmt.Fprintf(&sb, ", built %s", at.UTC().Format("2006-01-02 15:04 UTC"))
	}
	fmt.Fprintf(&sb, ", %d templates.\n", len(list.Templates))
	if at, ok := list.LastCheckedAt.Get(); ok {
		fmt.Fprintf(&sb, "Last checked %s: %s.\n", at.UTC().Format("2006-01-02 15:04 UTC"), list.LastOutcome.Or(""))
	}
	tw := tabwriter.NewWriter(&sb, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTITLE\tREVISION\tCATEGORIES\tSOURCE\tTRUST\tINSTALLED")
	for _, e := range list.Templates {
		installed := "no"
		if e.Installed {
			installed = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", safeText(e.ID), safeText(e.Title), e.Revision, safeText(strings.Join(e.Categories, ",")), safeText(e.Source), catalogSourceBadge(e.SourceKind, e.Signed), installed)
	}
	_ = tw.Flush()
	return sb.String()
}

func catalogTemplateSummary(t *apiv1.CatalogTemplate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s), revision %d, from the %s source (%s).\n", safeText(t.Title), safeText(t.ID), t.Revision, safeText(t.Source), catalogSourceBadge(t.SourceKind, t.Signed))
	if t.SourceKind == apiv1.CatalogSourceKindUserAdded && !t.Signed {
		fmt.Fprintln(&sb, "This source is unsigned: nothing proves the template came from its publisher.")
	}
	fmt.Fprintf(&sb, "Categories: %s\nDocumentation: %s\n", safeText(strings.Join(t.Categories, ", ")), safeText(t.Docs))
	if len(t.Privileges) == 0 {
		fmt.Fprintln(&sb, "Privileges: none beyond an ordinary container.")
	} else {
		fmt.Fprintln(&sb, "Privileges this template asks for:")
		for _, p := range t.Privileges {
			sb.WriteString(privilegeLine(p))
		}
	}
	fmt.Fprintf(&sb, "\ncompose.yaml:\n%s", safeBlock(t.Compose))
	if !strings.HasSuffix(t.Compose, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}
