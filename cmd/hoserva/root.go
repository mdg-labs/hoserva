package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var socketPath string

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hoserva",
		Short: "Hoserva command-line interface",
	}
	cmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Emit machine-readable JSON")
	cmd.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "Path to hoservad's Unix socket")
	cmd.AddCommand(
		statusCmd(),
		arrayCmd(),
		poolCmd(),
		diskCmd(),
		syncCmd(),
		scrubCmd(),
		fixCmd(),
		jobsCmd(),
		logsCmd(),
		configCmd(),
		doctorCmd(),
		userCmd(),
		updateCmd(),
		rollbackCmd(),
		rebootCmd(),
	)
	return cmd
}

func runCLI() int {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "hoserva: %v\n", err)
		return exitError
	}
	return exitOK
}

func apiCtx() context.Context {
	return context.Background()
}
