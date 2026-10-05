package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// migrateContainersCmd is `hoserva migrate containers` (doc 05 §4 steps 19 and
// 20): the user's templates and Compose Manager projects become stopped stacks,
// which are then started one at a time, each checked and confirmed to see its
// data before the next.
func migrateContainersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "containers",
		Short: "Create the stacks of your Unraid containers and start them one at a time",
		Long: "Phase D of the migration (doc 05 §4 steps 19 and 20), after the point of no return. Without a subcommand it lists what " +
			"the scan offers and what has been created: the templates grouped by class (the ones on Unraid's autostart list are " +
			"pre-selected, in its order), the Compose Manager projects, which are offered with their own compose.yaml, and the " +
			"containers created by hand, which nothing is generated for. `create` makes a stopped stack for each selected " +
			"template or project after printing its Compose and every warning; `start` starts one stack as a job; `check` reads " +
			"whether the started containers see their data; `confirm` offers the next. Nothing is created or started before the " +
			"parity initialisation has finished.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			list, err := c.ListMigrationContainers(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(list)
				return nil
			}
			printMigrationContainers(os.Stdout, list)
			return nil
		},
	}
	cmd.AddCommand(migrateContainersCreateCmd(), migrateContainersStartCmd(), migrateContainersCheckCmd(), migrateContainersConfirmCmd())
	return cmd
}

func printMigrationContainers(w io.Writer, l *apiv1.MigrationContainers) {
	if !l.ParityInitialized {
		_, _ = fmt.Fprint(w, "The migration is not past its point of no return: nothing can be created or started until the parity initialisation has finished (`hoserva migrate initialize-parity`).\n\n")
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TEMPLATE\tNAME\tCLASS\tSTATUS\tWARNINGS\tSELECTED\tSTACK")
	for _, t := range l.Templates {
		sel := ""
		switch {
		case t.Created:
			sel = "created"
		case t.Preselected:
			sel = "yes"
		case !t.Creatable:
			sel = "cannot be created"
		}
		class := string(t.Class)
		if p := t.AutostartPosition.Or(0); p > 0 {
			class = fmt.Sprintf("%s (#%d)", class, p)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", t.File, t.Name, class, t.Status, t.WarningCount, sel, t.Stack.Or("-"))
	}
	for _, p := range l.ComposeProjects {
		sel := ""
		switch {
		case p.Created:
			sel = "created"
		case !p.Creatable:
			sel = "cannot be created"
		}
		_, _ = fmt.Fprintf(tw, "%s\tCompose Manager project\t\t%s\t\t%s\t%s\n", p.Name, p.Status, sel, p.Stack.Or("-"))
	}
	_ = tw.Flush()
	for _, t := range l.Templates {
		if e, ok := t.Error.Get(); ok {
			_, _ = fmt.Fprintf(w, "\n%s could not be converted: %s\n", t.File, e)
		}
	}
	if len(l.ByHand) > 0 {
		_, _ = fmt.Fprintln(w, "\nNo template, recreate by hand (nothing is generated for these):")
		for _, b := range l.ByHand {
			_, _ = fmt.Fprintf(w, "  %s  %s\n", b.Name, b.Image.Or("(image not in the capture)"))
		}
	}
	if len(l.Stacks) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nCreated stacks, in the order they are offered:")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STACK\tSTATE\tFROM\tUNRAID WAIT\tDATA CHECK")
	for _, s := range l.Stacks {
		wait := "-"
		if n := s.WaitSeconds.Or(0); n > 0 {
			wait = fmt.Sprintf("%ds", n)
		}
		check := "not run"
		switch {
		case s.Checked && s.CheckFailed:
			check = "found a problem"
		case s.Checked:
			check = "ok"
		}
		state := string(s.State)
		if s.Awaiting {
			state += " (awaiting confirmation)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, state, s.Source, wait, check)
	}
	_ = tw.Flush()
	if a, ok := l.Awaiting.Get(); ok {
		_, _ = fmt.Fprintf(w, "\n%s is started and not confirmed: run `hoserva migrate containers check %s` and `confirm %s`, or stop it, before starting another.\n", a, a, a)
	} else if n, ok := l.Next.Get(); ok {
		_, _ = fmt.Fprintf(w, "\nNext: `hoserva migrate containers start %s`.\n", n)
	}
}

func migrateContainersCreateCmd() *cobra.Command {
	var acknowledge []string
	var yes bool
	cmd := &cobra.Command{
		Use:   "create [NAME...] [--acknowledge NAME]... --yes",
		Short: "Create stopped stacks from templates and Compose Manager projects, after showing their Compose and warnings",
		Long: "Each NAME is a template's file name (my-notes.xml) or a Compose Manager project's name, as `hoserva migrate containers` " +
			"lists them; without one the pre-selected templates (Unraid's autostart list) are used. The generated Compose and every " +
			"warning of each is printed first, including the writable-layer warning every conversion carries: this is the last point " +
			"in the sequence where it can be acted on. Without --yes nothing is created. A template whose conversion has warnings " +
			"that need manual action is created only if it is also named with --acknowledge. Stacks are created stopped, one by one; " +
			"a stack that fails leaves the ones before it created, and running the command again creates only what is missing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			names := slices.Clone(args)
			if len(names) == 0 {
				list, err := c.ListMigrationContainers(apiCtx())
				if err != nil {
					return mapAPIErr(err)
				}
				for _, t := range list.Templates {
					if t.Preselected {
						names = append(names, t.File)
					}
				}
				if len(names) == 0 {
					return fmt.Errorf("nothing is pre-selected: name the templates or projects to create (see `hoserva migrate containers`)")
				}
			}
			for _, a := range acknowledge {
				if !slices.Contains(names, a) {
					return fmt.Errorf("--acknowledge %s names something that is not being created", a)
				}
			}
			for _, n := range names {
				pv, err := c.GetMigrationTemplate(apiCtx(), apiv1.GetMigrationTemplateParams{Name: n})
				if err != nil {
					return mapAPIErr(err)
				}
				if !jsonOutput {
					fmt.Print(migrationPreviewReport(pv))
					fmt.Println()
				}
			}
			if !yes {
				return fmt.Errorf("nothing was created: read the Compose and the warnings above, then run it again with --yes (and --acknowledge NAME for each template whose warnings you accept)")
			}
			items := make([]apiv1.MigrationStackSelection, 0, len(names))
			for _, n := range names {
				it := apiv1.MigrationStackSelection{Name: n}
				if slices.Contains(acknowledge, n) {
					it.Acknowledged = apiv1.NewOptBool(true)
				}
				items = append(items, it)
			}
			res, err := c.CreateMigrationStacks(apiCtx(), &apiv1.MigrationStacksRequest{Items: items})
			if err != nil {
				return mapAPIErr(err)
			}
			failed := 0
			for _, r := range res.Results {
				if r.Status == apiv1.MigrationStackResultStatusFailed {
					failed++
				}
			}
			if jsonOutput {
				emit(res)
			} else {
				printStackResults(os.Stdout, res)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d stacks could not be created; the others were created, and running this again creates only what is missing", failed, len(res.Results))
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&acknowledge, "acknowledge", nil, "A template whose warnings you have read and accept; repeatable. Needed for a conversion with a warning that needs manual action")
	cmd.Flags().BoolVar(&yes, "yes", false, "Create the stacks after reading what was printed (required to act)")
	return cmd
}

func printStackResults(w io.Writer, res *apiv1.MigrationStacksCreated) {
	for _, r := range res.Results {
		switch r.Status {
		case apiv1.MigrationStackResultStatusCreated:
			_, _ = fmt.Fprintf(w, "created stack %s from %s (stopped)\n", r.Stack, r.Name)
		case apiv1.MigrationStackResultStatusAlreadyCreated:
			_, _ = fmt.Fprintf(w, "stack %s from %s was already created\n", r.Stack, r.Name)
		default:
			e, _ := r.Error.Get()
			_, _ = fmt.Fprintf(w, "stack %s from %s was NOT created: %s (%s)\n", r.Stack, r.Name, e.Message, e.Code)
		}
	}
	_, _ = fmt.Fprintln(w, "\nStart them one at a time with `hoserva migrate containers start NAME`, in the order `hoserva migrate containers` lists them.")
}

func migrateContainersStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start NAME",
		Short: "Start one created stack and wait for its start job",
		Long: "Starts the stack as a job and waits for it. Only one migrated container is started at a time: it is refused while " +
			"another that was started is neither confirmed nor stopped. When the job has finished, give the container the time " +
			"Unraid's autostart list gave it (printed), then run `hoserva migrate containers check NAME`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			j, err := c.StartMigrationContainer(apiCtx(), apiv1.StartMigrationContainerParams{Name: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done, err := waitForJob(ctx, c, j.ID)
			if err != nil {
				return err
			}
			if jsonOutput {
				emit(done)
			}
			switch done.Status {
			case apiv1.JobStatusSucceeded:
			case apiv1.JobStatusFailed:
				if e, ok := done.Error.Get(); ok && e.Message != "" {
					return fmt.Errorf("starting stack %s failed (job %s): %s", args[0], done.ID, e.Message)
				}
				return fmt.Errorf("starting stack %s failed (job %s)", args[0], done.ID)
			default:
				return fmt.Errorf("starting stack %s ended %s (job %s)", args[0], done.Status, done.ID)
			}
			if jsonOutput {
				return nil
			}
			fmt.Printf("Stack %s was started.", args[0])
			if list, err := c.ListMigrationContainers(apiCtx()); err == nil {
				for _, s := range list.Stacks {
					if s.Name == args[0] {
						if n := s.WaitSeconds.Or(0); n > 0 {
							fmt.Printf(" Unraid's autostart list waited %d seconds after this container before the next; give it as long.", n)
						}
					}
				}
			}
			fmt.Printf("\nThen run `hoserva migrate containers check %s`.\n", args[0])
			return nil
		},
	}
}

func migrateContainersCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check NAME",
		Short: "Check that a started stack's containers see their data",
		Long: "Reports, for each bind mount of the stack's containers under /mnt/user or /mnt/cache, whether the host path exists and " +
			"is not empty. It reads one directory entry of each path, which can spin a disk up, and runs only when you ask. It exits " +
			"non-zero when a path is missing, empty or unreadable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			res, err := c.CheckMigrationContainer(apiCtx(), apiv1.CheckMigrationContainerParams{Name: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(res)
			} else {
				printDataCheck(os.Stdout, res)
			}
			if !res.AllOk {
				return fmt.Errorf("the data check of stack %s found a path that is missing, empty or unreadable", args[0])
			}
			return nil
		},
	}
}

func printDataCheck(w io.Writer, c *apiv1.MigrationContainerCheck) {
	running := "running"
	if !c.Running {
		running = "NOT running"
	}
	_, _ = fmt.Fprintf(w, "Stack %s: %s\n", c.Stack, running)
	if len(c.Paths) == 0 {
		_, _ = fmt.Fprintln(w, "No bind mount under /mnt/user or /mnt/cache: nothing to check.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATUS\tPATH\tSEEN AS\tCONTAINER")
	for _, p := range c.Paths {
		status := string(p.Status)
		if e := p.Error.Or(""); e != "" {
			status += " (" + strings.TrimSpace(e) + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", status, p.Path, p.Destination, p.Container)
	}
	_ = tw.Flush()
	if c.AllOk {
		_, _ = fmt.Fprintf(w, "\nEvery path exists and holds data. If that is what you expect, `hoserva migrate containers confirm %s`.\n", c.Stack)
	}
}

func migrateContainersConfirmCmd() *cobra.Command {
	var accept bool
	cmd := &cobra.Command{
		Use:   "confirm NAME [--accept-failed-check]",
		Short: "Confirm a started stack sees its data, which offers the next",
		Long: "Records that you read the data check and the started container sees its data, so the next container can be started. " +
			"It needs a data check since the start. A check that found a path that is missing, empty or unreadable is confirmed " +
			"only with --accept-failed-check (a container whose data directory is meant to be empty); otherwise stop the container.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			req := apiv1.OptMigrationContainerConfirmRequest{}
			if accept {
				req = apiv1.NewOptMigrationContainerConfirmRequest(apiv1.MigrationContainerConfirmRequest{AcceptFailedCheck: apiv1.NewOptBool(true)})
			}
			st, err := c.ConfirmMigrationContainer(apiCtx(), req, apiv1.ConfirmMigrationContainerParams{Name: args[0]})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(st)
				return nil
			}
			fmt.Printf("Stack %s is confirmed.\n", st.Name)
			if list, err := c.ListMigrationContainers(apiCtx()); err == nil {
				if n, ok := list.Next.Get(); ok {
					fmt.Printf("Next: `hoserva migrate containers start %s`.\n", n)
				} else {
					fmt.Println("Every created stack is confirmed.")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&accept, "accept-failed-check", false, "Confirm although the data check found a path that is missing, empty or unreadable")
	return cmd
}
