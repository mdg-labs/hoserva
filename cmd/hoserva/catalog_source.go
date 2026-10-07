package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// catalogSourceCmd is `hoserva catalog source` (doc 04 §4): the catalog source
// URLs the user added beside the curated catalog, read and changed through
// the same operations the web UI uses.
func catalogSourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Add, refresh and remove your own catalog sources (doc 04 §4)"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the curated catalog and every catalog source you added",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := newAPIClient()
				if err != nil {
					return err
				}
				list, err := c.ListCatalogSources(apiCtx())
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(list)
					return nil
				}
				fmt.Print(catalogSourceListSummary(list))
				return nil
			},
		},
		catalogSourceAddCmd(),
		&cobra.Command{
			Use:   "refresh SOURCE-ID",
			Short: "Check a catalog source for a newer archive now",
			Long: "Runs one check of the source (the id `hoserva catalog source list` shows; `hoserva` is the curated catalog). " +
				"A source is only ever fetched when it is added or refreshed here. A failed check keeps the installed copy. " +
				"Exits non-zero when the check failed.",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := newAPIClient()
				if err != nil {
					return err
				}
				res, err := c.RefreshCatalogSource(apiCtx(), apiv1.RefreshCatalogSourceParams{ID: args[0]})
				if err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(res)
				} else if res.Outcome != apiv1.CatalogCheckOutcomeFailed {
					fmt.Print(catalogRefreshSummary(res))
				}
				if res.Outcome == apiv1.CatalogCheckOutcomeFailed {
					return fmt.Errorf("the check of source %s failed (%s): %s", args[0], res.Reason.Or(""), res.Message.Or(""))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "remove SOURCE-ID",
			Short: "Remove a catalog source you added and its downloaded copy",
			Long: "Removes the source's downloaded copy and its record, and nothing of any other source. " +
				"Apps already installed from it are not touched. The curated catalog cannot be removed.",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := newAPIClient()
				if err != nil {
					return err
				}
				if err := c.RemoveCatalogSource(apiCtx(), apiv1.RemoveCatalogSourceParams{ID: args[0]}); err != nil {
					return mapAPIErr(err)
				}
				if jsonOutput {
					emit(map[string]any{"removed": args[0]})
					return nil
				}
				fmt.Printf("Removed catalog source %s.\n", args[0])
				return nil
			},
		},
	)
	return cmd
}

func catalogSourceAddCmd() *cobra.Command {
	var (
		publicKey     string
		publicKeyFile string
	)
	cmd := &cobra.Command{
		Use:   "add URL",
		Short: "Add a catalog source URL of your own and fetch its catalog",
		Long: "Fetches the catalog archive from beneath URL (an https address) in the same format as the curated catalog, and records the source only once it is installed. " +
			"Without a public key the source is unsigned: nothing proves its archive came from its publisher, and its templates are badged unsigned. " +
			"With --public-key (the base64 of the Ed25519 key) or --public-key-file (a PEM file) the archive's detached signature must verify against that key.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := apiv1.AddCatalogSourceRequest{URL: args[0]}
			key := publicKey
			if publicKeyFile != "" {
				text, err := os.ReadFile(publicKeyFile)
				if err != nil {
					return fmt.Errorf("reading the public key file: %w", err)
				}
				key = string(text)
			}
			if cmd.Flags().Changed("public-key") || cmd.Flags().Changed("public-key-file") {
				if strings.TrimSpace(key) == "" {
					return fmt.Errorf("the public key is empty; leave --public-key and --public-key-file out to add an unsigned source")
				}
				req.PublicKey = apiv1.NewOptString(key)
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			src, err := c.AddCatalogSource(apiCtx(), &req)
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(src)
				return nil
			}
			fmt.Printf("Added catalog source %s (%s).\n", safeText(src.ID), catalogSourceBadge(src.Kind, src.Signed))
			if !src.Signed {
				fmt.Println("This source is unsigned: nothing proves its archive came from its publisher. Its templates are badged unsigned and still show every privilege they ask for.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&publicKey, "public-key", "", "The Ed25519 public key the archive is signed with, as base64 of the raw 32 bytes")
	cmd.Flags().StringVar(&publicKeyFile, "public-key-file", "", "A PEM file holding the Ed25519 public key the archive is signed with")
	cmd.MarkFlagsMutuallyExclusive("public-key", "public-key-file")
	return cmd
}

func catalogSourceBadge(kind apiv1.CatalogSourceKind, signed bool) string {
	trust := "unsigned"
	if signed {
		trust = "signed"
	}
	if kind == apiv1.CatalogSourceKindCurated {
		return "curated, " + trust
	}
	return "user-added, " + trust
}

func catalogSourceListSummary(list *apiv1.CatalogSourceList) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tTRUST\tSERIAL\tLAST REFRESHED\tURL")
	for _, s := range list.Sources {
		kind, trust, _ := strings.Cut(catalogSourceBadge(s.Kind, s.Signed), ", ")
		serial, refreshed := "-", "never"
		if v, ok := s.Serial.Get(); ok {
			serial = fmt.Sprint(v)
		}
		if at, ok := s.LastRefreshedAt.Get(); ok {
			refreshed = at.UTC().Format("2006-01-02 15:04 UTC")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", safeText(s.ID), kind, trust, serial, refreshed, safeText(s.URL))
	}
	_ = tw.Flush()
	return sb.String()
}

// stackTemplateUpdateCmd is `hoserva stack template-update` (doc 04 §7): whether
// the template a stack was installed from has a newer revision, with the diff
// against the stack's docker-compose.yml. It changes nothing.
func stackTemplateUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "template-update NAME",
		Short: "Show whether a stack's template has a newer revision, with a diff against its docker-compose.yml",
		Long: "Compares the revision the stack was installed from with the one its source lists now and prints a unified diff " +
			"from the stack's docker-compose.yml to the newer revision. It never changes the stack: a catalog update is only offered, and applying it is yours to do.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			u, err := c.GetStackTemplateUpdate(apiCtx(), apiv1.GetStackTemplateUpdateParams{Name: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(u)
				return nil
			}
			fmt.Print(stackTemplateUpdateSummary(args[0], u))
			return nil
		},
	}
}

func stackTemplateUpdateSummary(name string, u *apiv1.StackTemplateUpdate) string {
	var sb strings.Builder
	switch u.Status {
	case apiv1.StackTemplateUpdateStatusUpdateAvailable:
		fmt.Fprintf(&sb, "Template update available for stack %s: revision %d is installed, revision %d is available from source %s (%s).\n",
			safeText(name), u.InstalledRevision.Or(0), u.AvailableRevision.Or(0), safeText(u.Source.Or("")), catalogSourceBadge(u.SourceKind.Or(apiv1.CatalogSourceKindUserAdded), u.Signed.Or(false)))
		if u.ManuallyEdited {
			sb.WriteString("This stack's Compose file was edited by hand: the diff is against the edited file, and applying the update would overwrite those edits.\n")
		}
		if !u.Signed.Or(false) {
			sb.WriteString("The source is unsigned: nothing proves this revision came from its publisher.\n")
		}
		sb.WriteString("Nothing was changed.\n\n")
		sb.WriteString(safeBlock(u.Diff.Or("")))
	case apiv1.StackTemplateUpdateStatusUpToDate:
		fmt.Fprintf(&sb, "Stack %s is up to date: revision %d is installed and the source lists revision %d.\n", safeText(name), u.InstalledRevision.Or(0), u.AvailableRevision.Or(0))
	case apiv1.StackTemplateUpdateStatusNotFromTemplate:
		fmt.Fprintf(&sb, "Stack %s was not installed from a template, so there is nothing to compare.\n", safeText(name))
	case apiv1.StackTemplateUpdateStatusSourceRemoved:
		fmt.Fprintf(&sb, "The catalog source stack %s was installed from (%s) is no longer a source, so no update can be offered.\n", safeText(name), safeText(u.Source.Or("")))
	case apiv1.StackTemplateUpdateStatusTemplateRemoved:
		fmt.Fprintf(&sb, "Source %s no longer lists the template %s, so no update can be offered.\n", safeText(u.Source.Or("")), safeText(u.TemplateId.Or("")))
	}
	return sb.String()
}
