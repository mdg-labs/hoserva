package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	socketPath string
	// remoteHost, remotePort, remoteToken and insecureSkipTLSVerify are
	// #50's remote-use flags (Q43): once --host is set, every command
	// talks TLS to hoservad's :8008 (Q9) with a personal API token instead
	// of the local Unix socket. remoteToken defaults from the
	// HOSERVA_TOKEN environment variable so a token need not appear in
	// shell history or a process listing.
	remoteHost            string
	remotePort            int
	remoteToken           string
	insecureSkipTLSVerify bool
)

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hoserva",
		Short: "Hoserva command-line interface",
	}
	cmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Emit machine-readable JSON")
	cmd.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "Path to hoservad's Unix socket")
	cmd.PersistentFlags().StringVar(&remoteHost, "host", "", "Remote hoservad host, for TLS use over TCP instead of the local Unix socket (Q43)")
	cmd.PersistentFlags().IntVar(&remotePort, "port", defaultRemotePort, "Remote hoservad TCP port (Q9)")
	cmd.PersistentFlags().StringVar(&remoteToken, "token", os.Getenv("HOSERVA_TOKEN"), "Personal API token for --host (Q43); defaults from HOSERVA_TOKEN")
	cmd.PersistentFlags().BoolVar(&insecureSkipTLSVerify, "insecure-skip-tls-verify", false, "Skip TLS certificate verification for --host (needed for hoservad's default self-signed certificate, Q9)")
	cmd.AddCommand(
		statusCmd(),
		arrayCmd(),
		poolCmd(),
		diskCmd(),
		shareCmd(),
		networkCmd(),
		syncCmd(),
		scrubCmd(),
		fixCmd(),
		moverCmd(),
		jobsCmd(),
		logsCmd(),
		configCmd(),
		doctorCmd(),
		userCmd(),
		tokenCmd(),
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
