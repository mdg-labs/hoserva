//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host: it is built with `go test -tags lab -c` and run inside the lab
// container by scripts/devenv/run-lab-tests.sh, where smbd, smbclient,
// smbpasswd, useradd and userdel are real. It drives the production
// SambaAccounts that NewAuthService wires — through the handler operations
// the daemon serves — and connects to a real smbd with smbclient (#596).

package api_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
)

const labSMBPassword = "hoserva-lab-smb-password"

func labSMBRun(t *testing.T, stdin string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

type labSMB struct {
	conf string
	dir  string
}

// startLabSMBD serves one share whose only valid users are those named, with
// the passdb and state directories smbd and smbpasswd use by default inside
// the lab container.
func startLabSMBD(t *testing.T, validUsers []string) *labSMB {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	for _, tool := range []string{"smbd", "smbclient", "smbpasswd", "pdbedit", "useradd", "userdel", "getent"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed in the lab image: %v", tool, err)
		}
	}
	dir, err := os.MkdirTemp(filepath.Join("/lab", id), "user-smb-")
	if err != nil {
		t.Fatalf("creating a working directory in the lab: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	rendered := config.RenderSambaConf([]config.SambaShare{{
		Name: "shared", Browseable: true,
		Access: &config.SambaAccess{ValidUsers: validUsers, WriteList: validUsers},
	}})
	var kept []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "include = ") {
			continue
		}
		if name, ok := strings.CutPrefix(line, "   path = /mnt/user/"); ok {
			shareDir := filepath.Join(dir, "data", name)
			if err := os.MkdirAll(shareDir, 0o770); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(shareDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(shareDir, 0, 100); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(shareDir, 0o2770); err != nil {
				t.Fatal(err)
			}
			line = "   path = " + shareDir
		}
		kept = append(kept, line)
		if line == "[global]" {
			for _, g := range []string{
				"log file = " + filepath.Join(dir, "smbd.log"),
				"bind interfaces only = yes",
				"interfaces = lo",
			} {
				kept = append(kept, "   "+g)
			}
		}
	}
	if err := os.MkdirAll("/run/samba", 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "smb.conf")
	if err := os.WriteFile(conf, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := labSMBRun(t, "", "testparm", "-s", conf); err != nil {
		t.Fatalf("testparm refuses the rendered smb.conf: %v: %s", err, out)
	}

	smbd := exec.Command("smbd", "-s", conf, "-F")
	if err := smbd.Start(); err != nil {
		t.Fatalf("starting smbd: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-smbd.Process.Pid, syscall.SIGTERM)
		_ = smbd.Process.Signal(syscall.SIGTERM)
		_ = smbd.Wait()
	})
	return &labSMB{conf: conf, dir: dir}
}

func (s *labSMB) client(t *testing.T, user, password, command string) (string, error) {
	t.Helper()
	return labSMBRun(t, "", "smbclient", "-s", s.conf, "//127.0.0.1/shared", "-U", user+"%"+password, "-c", command)
}

func (s *labSMB) waitUntilAnswering(t *testing.T, user, password string) error {
	t.Helper()
	var lastErr error
	for i := 0; i < 40; i++ {
		var out string
		if out, lastErr = s.client(t, user, password, "ls"); lastErr == nil {
			return nil
		} else if !strings.Contains(out, "Connection to 127.0.0.1 failed") && !strings.Contains(out, "CONNECTION_REFUSED") {
			return fmt.Errorf("%w: %s", lastErr, strings.TrimSpace(out))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return lastErr
}

func passwdEntry(t *testing.T, name string) []string {
	t.Helper()
	out, err := labSMBRun(t, "", "getent", "passwd", name)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(out), ":")
}

func inSambaPassdb(t *testing.T, name string) bool {
	t.Helper()
	out, err := labSMBRun(t, "", "pdbedit", "-L")
	if err != nil {
		t.Fatalf("pdbedit -L: %v: %s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+":") {
			return true
		}
	}
	return false
}

func TestLabUserSMB_PasswordGivesWorkingLogin(t *testing.T) {
	ctx := context.Background()
	base, _ := newAuthTestService(t)
	// The service as hoservad builds it: SambaAccounts is NewAuthService's own default.
	svc := api.NewAuthService(base.Store, base.MachineKey)
	h := &api.Handler{Auth: svc}

	suffix := strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10)
	name := "hsu-alice-" + suffix
	smb := startLabSMBD(t, []string{name})

	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: name})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = h.DeleteUser(context.Background(), apiv1.DeleteUserParams{UserId: created.ID}) })

	if err := h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: labSMBPassword}, apiv1.SetUserPasswordParams{UserId: created.ID}); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	if err := smb.waitUntilAnswering(t, name, labSMBPassword); err != nil {
		log, _ := os.ReadFile(filepath.Join(smb.dir, "smbd.log"))
		t.Fatalf("the user cannot log in over SMB after its password was set: %v\nsmbd.log:\n%s", err, log)
	}
	local := filepath.Join(smb.dir, "payload.txt")
	if err := os.WriteFile(local, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := smb.client(t, name, labSMBPassword, "put "+local+" alice.txt"); err != nil {
		t.Fatalf("the user cannot write to its share: %v: %s", err, out)
	}
	if _, err := smb.client(t, name, "wrong-"+labSMBPassword, "ls"); err == nil {
		t.Error("a wrong password logged in")
	}

	entry := passwdEntry(t, name)
	if len(entry) != 7 {
		t.Fatalf("no system account for %s after its password was set", name)
	}
	uid, _ := strconv.Atoi(entry[2])
	if uid < 30000 || uid > 39999 {
		t.Errorf("uid = %d, want one in the reserved range 30000-39999", uid)
	}
	if entry[3] != "100" {
		t.Errorf("primary gid = %s, want 100 (users, Q26)", entry[3])
	}
	if !strings.HasSuffix(entry[6], "/nologin") {
		t.Errorf("shell = %s, want nologin", entry[6])
	}
	if _, err := os.Stat(entry[5]); err == nil {
		t.Errorf("home directory %s exists, want none", entry[5])
	}
	shadow, err := labSMBRun(t, "", "getent", "shadow", name)
	if err != nil {
		t.Fatalf("getent shadow %s: %v: %s", name, err, shadow)
	}
	if hash := strings.Split(shadow, ":")[1]; !strings.HasPrefix(hash, "!") && !strings.HasPrefix(hash, "*") {
		t.Errorf("the system account has a usable password hash %q, want it locked", hash)
	}

	if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: strings.ToUpper(name)}); err == nil {
		t.Error("a name differing only in case was accepted")
	}

	if err := h.DeleteUser(ctx, apiv1.DeleteUserParams{UserId: created.ID}); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := smb.client(t, name, labSMBPassword, "ls"); err == nil {
		t.Error("a deleted user still logs in over SMB")
	}
	if inSambaPassdb(t, name) {
		t.Error("a deleted user is still in the Samba passdb")
	}
	if entry := passwdEntry(t, name); entry != nil {
		t.Errorf("a deleted user's system account remains: %v", entry)
	}
}

// A name a system account Hoserva did not create already holds is refused, and
// neither that account nor a Samba entry for it is touched, by setting the
// password or by deleting the user.
func TestLabUserSMB_ANameAForeignSystemAccountHoldsIsRefused(t *testing.T) {
	ctx := context.Background()
	base, _ := newAuthTestService(t)
	svc := api.NewAuthService(base.Store, base.MachineKey)
	h := &api.Handler{Auth: svc}
	startLabSMBD(t, nil)

	for _, name := range []string{"daemon", "root"} {
		before := passwdEntry(t, name)
		if before == nil {
			t.Fatalf("the lab image has no %s account to test against", name)
		}
		created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: name})
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
		err = h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: labSMBPassword}, apiv1.SetUserPasswordParams{UserId: created.ID})
		if status := labAPIStatus(t, h, err); status != 409 {
			t.Errorf("SetUserPassword(%s) = status %d (%v), want 409", name, status, err)
		}
		if inSambaPassdb(t, name) {
			t.Errorf("%s was added to the Samba passdb", name)
		}
		if err := h.DeleteUser(ctx, apiv1.DeleteUserParams{UserId: created.ID}); err != nil {
			t.Fatalf("DeleteUser(%s): %v", name, err)
		}
		if after := passwdEntry(t, name); strings.Join(after, ":") != strings.Join(before, ":") {
			t.Errorf("deleting the Hoserva user %s changed the system account: %v -> %v", name, before, after)
		}
	}
}

// Every shape of name Hoserva accepts is one useradd accepts, including the
// longest, and each yields a login.
func TestLabUserSMB_EveryAcceptedShapeOfNameGetsAnAccount(t *testing.T) {
	ctx := context.Background()
	base, _ := newAuthTestService(t)
	svc := api.NewAuthService(base.Store, base.MachineKey)
	h := &api.Handler{Auth: svc}
	suffix := strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10)
	names := []string{
		"_hsu" + suffix,
		"hsu.dot-dash_" + suffix,
		"hsu" + suffix + strings.Repeat("x", 64-3-len(suffix)),
	}
	if len(names[2]) != 64 {
		t.Fatalf("test name has %d characters, want 64", len(names[2]))
	}
	smb := startLabSMBD(t, names)
	for _, name := range names {
		created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: name})
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
		t.Cleanup(func() { _ = h.DeleteUser(context.Background(), apiv1.DeleteUserParams{UserId: created.ID}) })
		if err := h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: labSMBPassword}, apiv1.SetUserPasswordParams{UserId: created.ID}); err != nil {
			t.Fatalf("SetUserPassword(%s): %v", name, err)
		}
		if err := smb.waitUntilAnswering(t, name, labSMBPassword); err != nil {
			t.Errorf("%s cannot log in: %v", name, err)
		}
	}
}

func labAPIStatus(t *testing.T, h *api.Handler, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	return apiError(t, h, err).StatusCode
}
