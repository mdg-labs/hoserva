package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner execs an external command and returns its stdout — the same
// scriptable-fake shape internal/disk.Runner and internal/parity.Runner
// already use (doc 06 §2). ComposeVersion is its only caller here: the
// Compose spec has no stable Go API (doc 04's proposed approach), so it is
// always invoked as its own argv, never a shell (CLAUDE.md).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandRunner is the real Runner.
type CommandRunner struct{}

// Run execs name with args as its argv and returns its stdout. A non-zero
// exit is reported with the command's captured stderr, mirroring
// internal/disk.CommandRunner.Run.
func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := bytes.TrimSpace(exitErr.Stderr); len(stderr) > 0 {
				return out, fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return out, err
	}
	return out, nil
}

var _ Runner = CommandRunner{}

// composeMissingPluginPatterns are the substrings a Docker CLI's own error
// carries when the "compose" subcommand plugin is not installed, across
// the CLI versions this checks against. Verified against the host's own
// `docker --version` ("Docker version 29.8.1, build 4a63305d74") using
// `DOCKER_HOST=unix:///nonexistent.sock docker nosuchplugin version` —
// substituting "compose" for the probed name below, since the CLI's
// message names whatever subcommand was invoked, not "compose"
// specifically:
//
//   - current CLI (docker/cli v28+, including Docker's own apt repository
//     per D8): a missing plugin falls through to cobra, which reports
//     "docker: unknown command: docker compose" (docker/cli v28.5.2
//     cmd/docker/docker.go:493), exit 1.
//   - current CLI, invoked with a flag its built-in parser does not know
//     (e.g. the old `--short`): cobra reports "unknown flag: --short"
//     before it ever reaches the command-name check, exit 125. Kept as
//     defence in depth; ComposeVersion itself no longer passes `--short`
//     (below), so this path is not the one it exercises.
//   - old CLI (pre-v22, e.g. v20.10.24 cmd/docker/docker.go:46):
//     "docker: 'compose' is not a docker command."
var composeMissingPluginPatterns = []string{
	"unknown command: docker compose",
	"is not a docker command",
	"unknown flag: --short",
}

func composePluginMissing(err error) bool {
	msg := err.Error()
	for _, pattern := range composeMissingPluginPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// ComposeVersion runs `docker compose version` (never a shell, and never
// with `--short`: on a current CLI without the plugin, `--short` fails
// with "unknown flag: --short" before cobra even reaches the
// "unknown command" case `composeMissingPluginPatterns` matches on) and
// returns the installed Compose v2 plugin's version, parsed from its
// "Docker Compose version X.Y.Z" line. ErrComposeUnavailable wraps only
// the "plugin missing" case (composePluginMissing), distinguishing it from
// any other failure, so a caller can report doc 04 §3's exact warning
// rather than treating every failure alike.
func ComposeVersion(ctx context.Context, run Runner) (string, error) {
	out, err := run.Run(ctx, "docker", "compose", "version")
	if err != nil {
		if composePluginMissing(err) {
			return "", fmt.Errorf("%w: %v", ErrComposeUnavailable, err)
		}
		return "", fmt.Errorf("container: running docker compose version: %w", err)
	}
	ver, ok := parseComposeVersion(out)
	if !ok {
		return "", fmt.Errorf("container: could not parse docker compose version output: %q", strings.TrimSpace(string(out)))
	}
	return ver, nil
}

// parseComposeVersion extracts the version from `docker compose version`'s
// "Docker Compose version X.Y.Z" output: its last whitespace-separated
// field.
func parseComposeVersion(out []byte) (string, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	return fields[len(fields)-1], true
}
