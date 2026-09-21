package main

import (
	"strings"
	"testing"
)

func TestRootCmdHasRemoteFlags(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"host", "port", "token", "insecure-skip-tls-verify"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("root command has no --%s persistent flag", name)
		}
	}
}

func TestRootCmdHasTokenCommand(t *testing.T) {
	root := rootCmd()
	token, _, err := root.Find([]string{"token"})
	if err != nil {
		t.Fatalf("find token: %v", err)
	}
	for _, name := range []string{"create", "list", "revoke"} {
		if _, _, err := token.Find([]string{name}); err != nil {
			t.Fatalf("find token %s: %v", name, err)
		}
	}
}

// TestNewRemoteAPIClientRequiresToken is #50's own acceptance criterion:
// the remote CLI authenticates with a personal API token (Q43), so --host
// with no token at all must fail clearly rather than silently connecting
// unauthenticated.
func TestNewRemoteAPIClientRequiresToken(t *testing.T) {
	if _, err := newRemoteAPIClient("hoserva.example", defaultRemotePort, "", false); err == nil {
		t.Error("newRemoteAPIClient with no token should fail")
	} else if !strings.Contains(err.Error(), "--token") {
		t.Errorf("newRemoteAPIClient error = %v, want it to mention --token", err)
	}
}

func TestNewRemoteAPIClientBuildsHTTPSURL(t *testing.T) {
	c, err := newRemoteAPIClient("hoserva.example", 8008, "hspat_sometoken", false)
	if err != nil {
		t.Fatalf("newRemoteAPIClient: %v", err)
	}
	if c == nil {
		t.Fatal("expected a non-nil client")
	}
}

// TestRootCmdHostRequiresToken exercises newAPIClient's own dispatch
// through the CLI itself: --host with no --token must surface the same
// clear error at the command level, not just from newRemoteAPIClient in
// isolation.
func TestRootCmdHostRequiresToken(t *testing.T) {
	t.Setenv("HOSERVA_TOKEN", "")
	root := rootCmd()
	root.SetArgs([]string{"--host", "hoserva.example", "status"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Errorf("--host with no token = %v, want an error mentioning --token", err)
	}
}
