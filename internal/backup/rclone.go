package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RcloneInstallCommand is what an operator runs to get rclone on a Debian
// host (Q41: rclone is a Recommends:, so a box installed without it has
// local destinations only).
const RcloneInstallCommand = "sudo apt install rclone"

// ErrRcloneMissing is returned when a remote destination needs rclone and
// the binary is not on PATH. Its message carries the install command
// (Q41), so the API can show it instead of an opaque exec failure.
var ErrRcloneMissing = fmt.Errorf("backup: rclone is not installed — install it with: %s", RcloneInstallCommand)

// Each rclone invocation gets its own deadline, so a slow upload does not
// leave the verification and prune steps that follow it with no time. A
// copy is given rcloneCopyTimeout plus the time its size takes at
// rcloneMinRate, since an appdata archive can run to many gigabytes.
const (
	rcloneCopyTimeout  = 15 * time.Minute
	rcloneQuickTimeout = 2 * time.Minute
	rcloneMinRate      = 512 * 1024
)

func rcloneTransferTimeout(size int64) time.Duration {
	if size <= 0 {
		return rcloneCopyTimeout
	}
	return rcloneCopyTimeout + time.Duration(size/rcloneMinRate)*time.Second
}

// rcloneRemoteName is the name of the on-the-fly remote every non-"rclone"
// destination is configured under, through RCLONE_CONFIG_<NAME>_* in the
// process environment — never through a config file, and never through
// argv, so a credential is not visible in the process list.
const rcloneRemoteName = "HOSERVADEST"

// RcloneCommand is one rclone invocation: an argv (never a shell line),
// extra environment entries, and optional stdin.
type RcloneCommand struct {
	Args  []string
	Env   []string
	Stdin []byte
}

// RcloneRunner runs rclone. Run returns ErrRcloneMissing when the binary
// is not installed and *RcloneExitError when rclone ran and failed.
type RcloneRunner interface {
	Run(ctx context.Context, cmd RcloneCommand) ([]byte, error)
}

// RcloneExitError is a non-zero rclone exit. rclone's own exit codes
// include 3 (directory not found) and 4 (file not found), which the
// remote target treats as "already absent".
type RcloneExitError struct {
	Code   int
	Output string
}

func (e *RcloneExitError) Error() string {
	return fmt.Sprintf("rclone exited with code %d: %s", e.Code, e.Output)
}

// ExecRclone runs the rclone binary with exec.CommandContext and an argv.
type ExecRclone struct {
	// Binary defaults to "rclone", resolved on PATH.
	Binary string
}

const maxRcloneOutput = 512

func (e ExecRclone) Run(ctx context.Context, c RcloneCommand) ([]byte, error) {
	bin := e.Binary
	if bin == "" {
		bin = "rclone"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrRcloneMissing
		}
		return nil, fmt.Errorf("locating rclone: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.WaitDelay = 5 * time.Second
	if len(c.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out := strings.TrimSpace(stderr.String())
			if len(out) > maxRcloneOutput {
				out = out[len(out)-maxRcloneOutput:]
			}
			return nil, &RcloneExitError{Code: exitErr.ExitCode(), Output: out}
		}
		return nil, fmt.Errorf("running rclone: %w", err)
	}
	return stdout.Bytes(), nil
}

// rcloneNotFound reports whether err is rclone saying the directory or
// file is not there.
func rcloneNotFound(err error) bool {
	var exitErr *RcloneExitError
	return errors.As(err, &exitErr) && (exitErr.Code == 3 || exitErr.Code == 4)
}

// rcloneTarget is a destination reached through rclone: an archive is
// written with `rclone copy`, listed with `rclone lsjson` and removed
// with `rclone deletefile`.
type rcloneTarget struct {
	runner RcloneRunner
	env    []string
	dir    string
}

func (t *rcloneTarget) remoteFile(name string) string {
	if strings.HasSuffix(t.dir, ":") {
		return t.dir + name
	}
	return t.dir + "/" + name
}

func (t *rcloneTarget) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return t.runner.Run(ctx, RcloneCommand{Args: args, Env: t.env})
}

type rcloneFile struct {
	Name    string    `json:"Name"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

func (t *rcloneTarget) listFiles(ctx context.Context) ([]rcloneFile, error) {
	out, err := t.run(ctx, rcloneQuickTimeout, "lsjson", "--files-only", "--no-mimetype", t.dir)
	if err != nil {
		if rcloneNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %q: %w", t.dir, err)
	}
	var files []rcloneFile
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("parsing rclone listing of %q: %w", t.dir, err)
	}
	return files, nil
}

func (t *rcloneTarget) write(ctx context.Context, srcPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("reading archive to upload: %w", err)
	}
	if _, err := t.run(ctx, rcloneTransferTimeout(info.Size()), "copy", "--immutable", srcPath, t.dir); err != nil {
		return fmt.Errorf("uploading to %q: %w", t.dir, err)
	}
	files, err := t.listFiles(ctx)
	if err != nil {
		return fmt.Errorf("verifying upload: %w", err)
	}
	name := filepath.Base(srcPath)
	for _, f := range files {
		if f.Name != name {
			continue
		}
		if f.Size >= 0 && f.Size != info.Size() {
			return fmt.Errorf("verifying upload: %q is %d bytes on the destination, want %d", name, f.Size, info.Size())
		}
		return nil
	}
	return fmt.Errorf("verifying upload: %q is not on the destination after the copy", name)
}

func (t *rcloneTarget) files(ctx context.Context) ([]targetFile, error) {
	listed, err := t.listFiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []targetFile
	for _, f := range listed {
		if !f.IsDir {
			out = append(out, targetFile{name: f.Name, size: f.Size, modTime: f.ModTime})
		}
	}
	return out, nil
}

func (t *rcloneTarget) fetch(ctx context.Context, name, dstPath string) error {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("fetching %q: not a file name", name)
	}
	if _, err := os.Lstat(dstPath); err == nil {
		return fmt.Errorf("fetching %q: %q already exists", name, dstPath)
	}
	// A listing that fails leaves the size unknown and the copy the base
	// deadline; the copy reports why the remote cannot be read.
	listed, _ := t.listFiles(ctx)
	var size int64
	for _, f := range listed {
		if f.Name == name {
			size = f.Size
		}
	}
	if _, err := t.run(ctx, rcloneTransferTimeout(size), "copyto", t.remoteFile(name), dstPath); err != nil {
		return fmt.Errorf("downloading %q: %w", t.remoteFile(name), err)
	}
	return nil
}

func (t *rcloneTarget) list(ctx context.Context) ([]archiveEntry, error) {
	files, err := t.listFiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []archiveEntry
	for _, f := range files {
		if f.IsDir {
			continue
		}
		m := archiveNamePattern.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		out = append(out, archiveEntry{name: f.Name, modTime: f.ModTime, reason: Reason(m[3]), installation: m[1]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	return out, nil
}

func (t *rcloneTarget) remove(ctx context.Context, name string) error {
	if _, err := t.run(ctx, rcloneQuickTimeout, "deletefile", t.remoteFile(name)); err != nil && !rcloneNotFound(err) {
		return fmt.Errorf("removing %q: %w", t.remoteFile(name), err)
	}
	return nil
}

func (t *rcloneTarget) readBack(ctx context.Context, name string) ([]byte, error) {
	out, err := t.run(ctx, rcloneQuickTimeout, "cat", t.remoteFile(name))
	if err != nil {
		return nil, fmt.Errorf("reading %q back: %w", t.remoteFile(name), err)
	}
	return out, nil
}
