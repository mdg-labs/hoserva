package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// backupWaitInterval is how often --wait asks whether the job has finished.
var backupWaitInterval = time.Second

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Back up now"}
	run := &cobra.Command{Use: "run", Short: "Run a backup now"}
	var wait bool
	config := &cobra.Command{
		Use:   "config",
		Short: "Write a config backup to every enabled destination now (doc 10 §1)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			j, err := c.RunConfigBackup(apiCtx())
			if err != nil {
				return mapAPIErr(err)
			}
			if !wait {
				printQueuedJob(j)
				return nil
			}
			ctx, stop := signal.NotifyContext(apiCtx(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done, err := waitForJob(ctx, c, j.ID)
			if err != nil {
				return err
			}
			if jsonOutput {
				emit(done)
			} else {
				fmt.Printf("%s %s\n", done.ID, done.Status)
			}
			switch done.Status {
			case apiv1.JobStatusSucceeded:
				return nil
			case apiv1.JobStatusFailed:
				if e, ok := done.Error.Get(); ok && e.Message != "" {
					return fmt.Errorf("config backup %s failed: %s", done.ID, e.Message)
				}
				return fmt.Errorf("config backup %s failed", done.ID)
			default:
				return fmt.Errorf("config backup %s ended %s", done.ID, done.Status)
			}
		},
	}
	config.Flags().BoolVar(&wait, "wait", false, "Wait for the backup to finish, and fail if it did not succeed")
	run.AddCommand(config)
	cmd.AddCommand(run)
	return cmd
}

func printQueuedJob(j *apiv1.Job) {
	if jsonOutput {
		emit(j)
		return
	}
	fmt.Println(j.ID)
}

// waitForJob polls the job until it reaches a state it will not leave by
// itself: succeeded, failed, cancelled or interrupted.
func waitForJob(ctx context.Context, c *apiv1.Client, id uuid.UUID) (*apiv1.Job, error) {
	for {
		j, err := c.GetJob(ctx, apiv1.GetJobParams{JobId: id})
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("stopped waiting for job %s; it keeps running", id)
			}
			return nil, mapAPIErr(err)
		}
		switch j.Status {
		case apiv1.JobStatusSucceeded, apiv1.JobStatusFailed, apiv1.JobStatusCancelled, apiv1.JobStatusInterrupted:
			return j, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("stopped waiting for job %s; it keeps running", id)
		case <-time.After(backupWaitInterval):
		}
	}
}
