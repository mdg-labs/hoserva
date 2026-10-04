//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45): built
// with `go test -tags lab -c` on the host and run by `make test-lab`
// inside the lab container, where smbd, smbclient and smbpasswd exist.
// It starts a real smbd on a smb.conf RenderSambaConf produced from
// grants, and shows a client of each kind getting exactly the access the
// grants name (#594).

package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const labSambaPassword = "hoserva-lab-access"

func labSambaDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	dir, err := os.MkdirTemp(filepath.Join("/lab", id), "smb-access-")
	if err != nil {
		t.Fatalf("creating a working directory in the lab: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// MkdirTemp makes it 0700; the accounts under test must reach what is below.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func labRun(t *testing.T, stdin string, name string, args ...string) (string, error) {
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

func TestLabSamba_ServesOnlyWhatTheGrantsName(t *testing.T) {
	dir := labSambaDir(t)
	for _, tool := range []string{"smbd", "smbclient", "smbpasswd", "useradd", "userdel"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed in the lab image: %v", tool, err)
		}
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	alice, bob, carol := "hsa-alice-"+suffix, "hsa-bob-"+suffix, "hsa-carol-"+suffix
	conf := filepath.Join(dir, "smb.conf")
	for _, u := range []string{alice, bob, carol} {
		if out, err := labRun(t, "", "useradd", "-M", "-N", "-s", "/usr/sbin/nologin", "-g", "100", u); err != nil {
			t.Fatalf("useradd %s: %v: %s", u, err, out)
		}
		u := u
		t.Cleanup(func() { _, _ = labRun(t, "", "userdel", u) })
	}

	shares := []SambaShare{
		{Name: "shared", Browseable: true, Access: &SambaAccess{ValidUsers: []string{alice, bob}, WriteList: []string{alice}}},
		{Name: "shut", Browseable: true, Access: &SambaAccess{}},
	}
	rendered := RenderSambaConf(shares)
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
				"private dir = " + filepath.Join(dir, "private"),
				"lock directory = " + filepath.Join(dir, "lock"),
				"state directory = " + filepath.Join(dir, "lock"),
				"cache directory = " + filepath.Join(dir, "lock"),
				"pid directory = " + filepath.Join(dir, "run"),
				"log file = " + filepath.Join(dir, "smbd.log"),
				"passdb backend = tdbsam:" + filepath.Join(dir, "private", "passdb.tdb"),
				"bind interfaces only = yes",
				"interfaces = lo",
			} {
				kept = append(kept, "   "+g)
			}
		}
	}
	// smbd's RPC pipe directory is not one of the paths smb.conf moves.
	if err := os.MkdirAll("/run/samba", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"private", "lock", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(conf, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := labRun(t, "", "testparm", "-s", conf); err != nil {
		t.Fatalf("testparm refuses the rendered smb.conf: %v: %s", err, out)
	}
	for _, u := range []string{alice, bob, carol} {
		if out, err := labRun(t, labSambaPassword+"\n"+labSambaPassword+"\n", "smbpasswd", "-c", conf, "-a", "-s", u); err != nil {
			t.Fatalf("smbpasswd %s: %v: %s", u, err, out)
		}
	}

	smbd := exec.Command("smbd", "-s", conf, "-F")
	if err := smbd.Start(); err != nil {
		t.Fatalf("starting smbd: %v", err)
	}
	t.Cleanup(func() {
		// smbd puts itself and its helpers in a process group of its own.
		_ = syscall.Kill(-smbd.Process.Pid, syscall.SIGTERM)
		_ = smbd.Process.Signal(syscall.SIGTERM)
		_ = smbd.Wait()
	})
	for i := 0; ; i++ {
		if out, err := labRun(t, "", "smbclient", "-s", conf, "-L", "127.0.0.1", "-U", alice+"%"+labSambaPassword); err == nil {
			break
		} else if i == 40 {
			log, _ := os.ReadFile(filepath.Join(dir, "smbd.log"))
			t.Fatalf("smbd did not start answering: %v: %s\nsmbd.log:\n%s", err, out, log)
		}
		time.Sleep(250 * time.Millisecond)
	}

	local := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(local, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := func(share, user string, command string) error {
		out, err := labRun(t, "", "smbclient", "-s", conf, "//127.0.0.1/"+share, "-U", user+"%"+labSambaPassword, "-c", command)
		t.Logf("%s on %s as %s: err=%v out=%s", command, share, user, err, strings.TrimSpace(out))
		return err
	}

	if err := client("shared", alice, "put "+local+" alice.txt"); err != nil {
		t.Fatalf("the user granted read-write cannot write: %v", err)
	}
	if err := client("shared", bob, "ls alice.txt"); err != nil {
		t.Errorf("the user granted read-only cannot read: %v", err)
	}
	if err := client("shared", bob, "put "+local+" bob.txt"); err == nil {
		t.Error("the user granted read-only wrote to the share")
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "shared", "bob.txt")); err == nil {
		t.Error("the read-only user's file exists on disk")
	}
	if err := client("shared", carol, "ls"); err == nil {
		t.Error("a user with no grant reached the share")
	}
	if err := client("shared", carol, "put "+local+" carol.txt"); err == nil {
		t.Error("a user with no grant wrote to the share")
	}
	for _, u := range []string{alice, bob, carol} {
		if err := client("shut", u, "ls"); err == nil {
			t.Errorf("%s reached a share nobody is granted", u)
		}
	}
}
