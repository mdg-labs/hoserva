package main

import (
	"fmt"

	"github.com/spf13/cobra"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func appRegistryCredentialsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "registry-credentials",
		Short: "List the registries the daily update check has a saved login for",
		Long: "Lists registry hosts only: a saved password is never shown. " +
			"Save one with `hoserva app registry-credential-set`.",
		Args: cobra.NoArgs,
		RunE: runAPI(func(c *apiv1.Client) (any, error) { return c.ListRegistryCredentials(apiCtx()) }),
	}
}

func appRegistryCredentialSetCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "registry-credential-set REGISTRY",
		Short: "Save the login the daily update check uses for a registry",
		Long: "Saves a username and password for a registry host such as ghcr.io or registry.example.com:5000, " +
			"sealed under the server's machine key. The password is read from the terminal without echo, or from " +
			"standard input when it is redirected (`... | hoserva app registry-credential-set ghcr.io --username me`), " +
			"never from an argument. It is sent only to the registry's own host, over https.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			password, err := readSecret("Registry password: ")
			if err != nil {
				return err
			}
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			req := &apiv1.PutRegistryCredentialRequest{Username: username, Password: password}
			if err := c.PutRegistryCredential(apiCtx(), req, apiv1.PutRegistryCredentialParams{Registry: args[0]}); err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(map[string]any{"saved": args[0]})
				return nil
			}
			fmt.Printf("Saved the credential for %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "The account name to log in with")
	_ = cmd.MarkFlagRequired("username")
	return cmd
}

func appRegistryCredentialDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "registry-credential-delete REGISTRY",
		Short: "Delete the saved login for a registry; the update check asks it anonymously again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAPIClient()
			if err != nil {
				return err
			}
			if err := c.DeleteRegistryCredential(apiCtx(), apiv1.DeleteRegistryCredentialParams{Registry: args[0]}); err != nil {
				return mapAPIErr(err)
			}
			if jsonOutput {
				emit(map[string]any{"deleted": args[0]})
				return nil
			}
			fmt.Printf("Deleted the credential for %s.\n", args[0])
			return nil
		},
	}
}
