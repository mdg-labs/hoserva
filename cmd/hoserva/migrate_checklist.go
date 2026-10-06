package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// migrateChecklistCmd is `hoserva migrate checklist` (doc 05 §4 steps 18 and 21
// to 25): what is left to do once the migration has finished, each item shown
// as done only when the daemon's records say so. `ack ITEM` records the two
// items no record can show.
func migrateChecklistCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checklist",
		Short: "What is left to do after the migration",
		Long: "The closing steps of Phase D: appdata back on the cache, the initial sync, a full scrub after it, a tested notification " +
			"channel, the sync, scrub and mover schedules, the User Scripts inventory and a restore drill. An item is done when a record " +
			"shows it: a job that succeeded, a channel whose test succeeded, a schedule that is enabled. `ack ITEM` is for the two items " +
			"nothing records, user_scripts and restore_drill, and records who acknowledged them and when. The checklist applies once the " +
			"parity initialisation (`hoserva migrate initialize-parity`) has run and the migration is past its point of no return.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			list, err := c.GetMigrationChecklist(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(list)
				return nil
			}
			printMigrationChecklist(os.Stdout, list)
			return nil
		},
	}
	cmd.AddCommand(migrateChecklistAckCmd())
	return cmd
}

func migrateChecklistAckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ack ITEM",
		Short: "Acknowledge user_scripts or restore_drill",
		Long: "Records that you worked through the User Scripts inventory (user_scripts) or did a restore drill (restore_drill): deleted a " +
			"file that has not changed since the last sync, recovered it and compared its checksum with the one you took before. It is stored with who acknowledged it and when, and the restore drill's with the " +
			"latest fix job of one file (hoserva fix --path). Every other item is derived from a record and is refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			it, err := c.AcknowledgeMigrationChecklistItem(apiCtx(), apiv1.AcknowledgeMigrationChecklistItemParams{Item: apiv1.MigrationChecklistItemId(args[0])})
			if err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(it)
				return nil
			}
			_, _ = fmt.Fprintf(os.Stdout, "%s acknowledged by %s at %s\n", it.ID, it.AcknowledgedBy.Or("unknown"), it.AcknowledgedAt.Or(time.Time{}).Local().Format(time.RFC3339))
			return nil
		},
	}
}

var checklistTitles = map[apiv1.MigrationChecklistItemId]string{
	apiv1.MigrationChecklistItemIdAppdataCache:  "Appdata moved back onto the cache",
	apiv1.MigrationChecklistItemIdInitialSync:   "Initial sync complete",
	apiv1.MigrationChecklistItemIdFullScrub:     "Full scrub after the initial sync",
	apiv1.MigrationChecklistItemIdNotifications: "Notifications tested",
	apiv1.MigrationChecklistItemIdSchedules:     "Sync, scrub and mover schedules set",
	apiv1.MigrationChecklistItemIdUserScripts:   "User Scripts inventory reviewed",
	apiv1.MigrationChecklistItemIdRestoreDrill:  "Restore drill done",
}

// checklistTodo says what to do for an item that is not done.
var checklistTodo = map[apiv1.MigrationChecklistItemId]string{
	apiv1.MigrationChecklistItemIdAppdataCache: "Relocate the appdata share onto the cache: startShareRelocation, POST /shares/appdata/relocate with {\"to\":\"cache\"}.",
	apiv1.MigrationChecklistItemIdInitialSync:  "Until it has succeeded the array has no redundancy. `hoserva sync` starts one if none is running.",
	apiv1.MigrationChecklistItemIdFullScrub:    "`hoserva scrub --percent 100 --all-blocks` once the initial sync is done. Without --all-blocks a scrub skips blocks synced in the last 10 days, which is all of them right after a sync, so it does not count.",
	apiv1.MigrationChecklistItemIdUserScripts:  "Recreate what you still want as a cron job or systemd timer; Hoserva neither runs nor translates them. Then `hoserva migrate checklist ack user_scripts`.",
	apiv1.MigrationChecklistItemIdRestoreDrill: "Pick an unimportant file that has not changed since the last sync, note its checksum, delete it, recover it from parity with `hoserva fix --confirm --path /mnt/user/<share>/<file>` and compare the checksum. `--path` restores only that file; `hoserva fix` without it restores the whole array to its last sync and reverts every change since. Then `hoserva migrate checklist ack restore_drill`.",
}

func printMigrationChecklist(w io.Writer, l *apiv1.MigrationChecklist) {
	finishedAt, ok := l.FinishedAt.Get()
	if !l.Finished || !ok {
		_, _ = fmt.Fprintln(w, "The checklist does not apply: the migration has not finished. It applies to a migration that has passed its point of no return (`hoserva migrate initialize-parity`).")
		return
	}
	_, _ = fmt.Fprintf(w, "Migration finished %s.\n\n", finishedAt.Local().Format(time.RFC3339))
	for _, it := range l.Items {
		mark := "[ ]"
		switch it.Status {
		case apiv1.MigrationChecklistItemStatusDone:
			mark = "[x]"
		case apiv1.MigrationChecklistItemStatusNotApplicable:
			mark = "[-]"
		}
		title := checklistTitles[it.ID]
		if title == "" {
			title = string(it.ID)
		}
		_, _ = fmt.Fprintf(w, "%s %s (%s)\n", mark, title, it.ID)
		printChecklistEvidence(w, it)
	}
}

func printChecklistEvidence(w io.Writer, it apiv1.MigrationChecklistItem) {
	line := func(format string, args ...any) { _, _ = fmt.Fprintf(w, "      "+format+"\n", args...) }
	switch it.Status {
	case apiv1.MigrationChecklistItemStatusNotApplicable:
		line("Not applicable: the source had no cache disk, so appdata is already on the array.")
	case apiv1.MigrationChecklistItemStatusDone:
		switch {
		case it.AcknowledgedBy.IsSet():
			line("Acknowledged by %s at %s.", it.AcknowledgedBy.Or(""), it.AcknowledgedAt.Or(time.Time{}).Local().Format(time.RFC3339))
		case it.JobId.IsSet():
			line("Job %s, %s.", it.JobId.Or(""), it.DoneAt.Or(time.Time{}).Local().Format(time.RFC3339))
		default:
			line("Since %s.", it.DoneAt.Or(time.Time{}).Local().Format(time.RFC3339))
		}
		if it.ID == apiv1.MigrationChecklistItemIdRestoreDrill && it.JobId.IsSet() {
			line("Recorded with fix job %s.", it.JobId.Or(""))
		}
	default:
		if hint := checklistTodo[it.ID]; hint != "" {
			line("%s", hint)
		}
		if it.ID == apiv1.MigrationChecklistItemIdRestoreDrill && it.JobId.IsSet() {
			line("Latest fix job of one file: %s; an acknowledgement records it.", it.JobId.Or(""))
		}
	}
	if n, ok := it.Notifications.Get(); ok {
		printChecklistNotifications(line, it.Status, n)
	}
	if s, ok := it.Schedules.Get(); ok {
		printChecklistSchedules(line, s)
	}
	if it.ID == apiv1.MigrationChecklistItemIdUserScripts {
		if len(it.Scripts) == 0 {
			line("The scan listed no User Scripts. The migration report's User Scripts row says whether its entries could be found; none found is not a finding that there are none.")
		}
		for _, sc := range it.Scripts {
			line("User script %q, schedule %s", sc.Name, sc.Schedule.Or("none in customSchedule.cron"))
		}
	}
}

func printChecklistNotifications(line func(string, ...any), status apiv1.MigrationChecklistItemStatus, n apiv1.MigrationChecklistNotifications) {
	line("%d notification %s, %d enabled with a test that succeeded after the channel last changed.", n.Channels, plural(int(n.Channels), "channel", "channels"), n.Tested)
	if status != apiv1.MigrationChecklistItemStatusDone {
		line("Add a channel and send a test through it (Settings, Notifications). A test that fails does not count.")
	}
	if len(n.Agents) > 0 {
		line("Unraid notified through: %s. Recreate each as a channel; no secret is carried over.", strings.Join(n.Agents, ", "))
	}
}

func printChecklistSchedules(line func(string, ...any), s apiv1.MigrationChecklistSchedules) {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	line("Nightly chain from %s: mover %s, sync %s, scrub %s.", s.ChainStartTime, onOff(s.Mover), onOff(s.Sync), onOff(s.Scrub))
	line("Unraid has no sync schedule to carry over (its parity is updated as files are written): choose when the nightly sync runs.")
	o := s.Offers
	if v := o.MoverCron.Or(""); v != "" {
		if t := o.MoverTime.Or(""); t != "" {
			line("Unraid's mover ran %q: offered as the chain's start time, %s.", v, t)
		} else {
			line("Unraid's mover ran %q: set the chain's start time by hand.", v)
		}
	}
	if p, ok := o.ParityCheck.Get(); ok {
		kind := "correcting"
		if !p.Correcting {
			kind = "non-correcting (offered as a scrub that only reports)"
		}
		line("Unraid's parity check (mode %s, hour %s, %s) is offered as the scrub schedule.", p.Mode.Or("?"), p.Hour.Or("?"), kind)
	}
	if v := o.SpindownDelay.Or(""); v != "" {
		line("Unraid's spin-down delay was %s: offered as Hoserva's default.", v)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
