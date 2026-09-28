package container

import (
	"context"
	"errors"
	"testing"
)

func TestComposeVersion(t *testing.T) {
	runner := NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, []byte("Docker Compose version 2.29.7\n"), nil)

	got, err := ComposeVersion(context.Background(), runner)
	if err != nil {
		t.Fatalf("ComposeVersion: %v", err)
	}
	if got != "2.29.7" {
		t.Fatalf("ComposeVersion() = %q, want 2.29.7", got)
	}

	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "docker" {
		t.Fatalf("Calls() = %v, want one docker call", calls)
	}
	wantArgs := []string{"compose", "version"}
	if len(calls[0].Args) != len(wantArgs) {
		t.Fatalf("Args = %v, want %v", calls[0].Args, wantArgs)
	}
	for i, a := range wantArgs {
		if calls[0].Args[i] != a {
			t.Fatalf("Args = %v, want %v", calls[0].Args, wantArgs)
		}
	}
}

// TestComposeVersion_MissingPlugin_CurrentCLI proves a missing Compose v2
// plugin is reported through ErrComposeUnavailable against the message a
// current Docker CLI actually prints (doc 04 §3's acceptance test). The
// fixture is verbatim stderr captured on the host's own `docker --version`
// ("Docker version 29.8.1, build 4a63305d74") by running
// `DOCKER_HOST=unix:///nonexistent.sock docker nosuchplugin version`,
// substituting "compose" for "nosuchplugin" (the CLI's message names
// whatever subcommand was invoked, not "compose" specifically). Without
// this fixture, ComposeVersion's old check — which only matched the
// pre-v22 "is not a docker command" string — silently fell through to
// "some other failure" on every current install.
func TestComposeVersion_MissingPlugin_CurrentCLI(t *testing.T) {
	runner := NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil,
		errors.New("docker: unknown command: docker compose\n\nRun 'docker --help' for more information"))

	_, err := ComposeVersion(context.Background(), runner)
	if !errors.Is(err, ErrComposeUnavailable) {
		t.Fatalf("ComposeVersion() error = %v, want ErrComposeUnavailable", err)
	}
}

// TestComposeVersion_MissingPlugin_OldCLI proves the pre-v22 wording is
// still recognised, so a host that has not upgraded its CLI keeps getting
// the correct warning too.
func TestComposeVersion_MissingPlugin_OldCLI(t *testing.T) {
	runner := NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil, errors.New(`docker: 'compose' is not a docker command`))

	_, err := ComposeVersion(context.Background(), runner)
	if !errors.Is(err, ErrComposeUnavailable) {
		t.Fatalf("ComposeVersion() error = %v, want ErrComposeUnavailable", err)
	}
}

// TestComposeVersion_MissingPlugin_UnknownFlag proves the defence-in-depth
// path: an "unknown flag" failure (what a current CLI prints if a caller
// ever passes `--short` again, verbatim from
// `DOCKER_HOST=unix:///nonexistent.sock docker nosuchplugin version
// --short` on the same host CLI, "unknown flag: --short" exit 125) is
// still treated as the plugin being missing, not as an unrelated failure.
func TestComposeVersion_MissingPlugin_UnknownFlag(t *testing.T) {
	runner := NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil,
		errors.New("unknown flag: --short\n\nUsage:  docker [OPTIONS] COMMAND [ARG...]\n\nRun 'docker --help' for more information"))

	_, err := ComposeVersion(context.Background(), runner)
	if !errors.Is(err, ErrComposeUnavailable) {
		t.Fatalf("ComposeVersion() error = %v, want ErrComposeUnavailable", err)
	}
}

// TestComposeVersion_OtherFailureNotWrapped proves a failure that is not
// the Compose plugin being missing — a timeout, here — is reported as
// itself, not wrapped in ErrComposeUnavailable: a caller checking
// errors.Is(err, ErrComposeUnavailable) must be able to tell "the plugin
// is missing" apart from "the command failed for some other reason".
func TestComposeVersion_OtherFailureNotWrapped(t *testing.T) {
	runner := NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil, context.DeadlineExceeded)

	_, err := ComposeVersion(context.Background(), runner)
	if errors.Is(err, ErrComposeUnavailable) {
		t.Fatalf("ComposeVersion() error = %v, must not be ErrComposeUnavailable for a non-plugin failure", err)
	}
	if err == nil {
		t.Fatal("ComposeVersion() = nil, want the underlying failure surfaced")
	}
}
