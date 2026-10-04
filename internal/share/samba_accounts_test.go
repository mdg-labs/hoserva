package share

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const smbTestPassword = "correct horse battery staple"

func newSmbpasswdForTest(script func(FakeCall) (CommandResult, error)) (SmbpasswdAccounts, *FakeUnixAccounts, *FakeCommander) {
	unix := NewFakeUnixAccounts()
	cmd := &FakeCommander{Script: script}
	return SmbpasswdAccounts{Unix: unix, Cmd: cmd}, unix, cmd
}

func TestSmbpasswdAccounts_SetPasswordCreatesTheSystemAccountBeforeSmbpasswd(t *testing.T) {
	var accountExistedAtSmbpasswd bool
	var unix *FakeUnixAccounts
	acc, unix, cmd := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		if c.Name == "smbpasswd" {
			accountExistedAtSmbpasswd = unix.Has("alice")
		}
		return CommandResult{}, nil
	})
	if err := acc.SetPassword(context.Background(), "alice", smbTestPassword); err != nil {
		t.Fatal(err)
	}
	if !accountExistedAtSmbpasswd {
		t.Error("smbpasswd ran before the system account existed, which tdbsam refuses")
	}
	calls := cmd.Calls()
	if len(calls) != 1 || calls[0].Name != "smbpasswd" {
		t.Fatalf("commands = %+v, want one smbpasswd", calls)
	}
	if !reflect.DeepEqual(calls[0].Args, []string{"-a", "-s", "alice"}) {
		t.Errorf("smbpasswd argv = %q", calls[0].Args)
	}
	if calls[0].Stdin != smbTestPassword+"\n"+smbTestPassword+"\n" {
		t.Errorf("smbpasswd stdin = %q, want the password twice", calls[0].Stdin)
	}
	for _, a := range calls[0].Args {
		if strings.Contains(a, smbTestPassword) {
			t.Error("the password is in smbpasswd's argv")
		}
	}
}

func TestSmbpasswdAccounts_SetPasswordRefusesAnInvalidNameBeforeAnyCommand(t *testing.T) {
	acc, unix, cmd := newSmbpasswdForTest(nil)
	err := acc.SetPassword(context.Background(), "x; rm -rf /", smbTestPassword)
	if !errors.Is(err, ErrInvalidAccountName) {
		t.Fatalf("SetPassword = %v, want ErrInvalidAccountName", err)
	}
	if len(cmd.Calls()) != 0 || len(unix.Events()) != 0 {
		t.Errorf("commands ran for an invalid name: %+v %v", cmd.Calls(), unix.Events())
	}
}

func TestSmbpasswdAccounts_SetPasswordRemovesAnAccountItCreatedWhenSmbpasswdFails(t *testing.T) {
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: "Failed to add entry for user alice."}, nil
	})
	err := acc.SetPassword(context.Background(), "alice", smbTestPassword)
	if err == nil || !strings.Contains(err.Error(), "Failed to add entry") {
		t.Fatalf("SetPassword = %v, want smbpasswd's failure", err)
	}
	if unix.Has("alice") {
		t.Error("the system account created for a failed SetPassword was left behind")
	}
}

func TestSmbpasswdAccounts_SetPasswordKeepsAnAccountItDidNotCreate(t *testing.T) {
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: "boom"}, nil
	})
	if _, err := unix.Ensure(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := acc.SetPassword(context.Background(), "alice", smbTestPassword); err == nil {
		t.Fatal("SetPassword succeeded though smbpasswd failed")
	}
	if !unix.Has("alice") {
		t.Error("a failed password change removed an account that already existed")
	}
}

func TestSmbpasswdAccounts_SetPasswordReportsAFailedCompensation(t *testing.T) {
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: "smbpasswd broke"}, nil
	})
	unix.FailRemove(errors.New("userdel broke"))
	err := acc.SetPassword(context.Background(), "alice", smbTestPassword)
	if err == nil || !strings.Contains(err.Error(), "smbpasswd broke") || !strings.Contains(err.Error(), "userdel broke") {
		t.Fatalf("SetPassword = %v, want both failures", err)
	}
}

func TestSmbpasswdAccounts_SetPasswordDoesNotRunSmbpasswdWhenTheAccountCannotBeMade(t *testing.T) {
	for name, setup := range map[string]func(*FakeUnixAccounts){
		"name held by a foreign account": func(u *FakeUnixAccounts) { u.HoldForeign("daemon") },
		"useradd failing":                func(u *FakeUnixAccounts) { u.FailEnsure(errors.New("useradd failed")) },
	} {
		t.Run(name, func(t *testing.T) {
			acc, unix, cmd := newSmbpasswdForTest(nil)
			setup(unix)
			if err := acc.SetPassword(context.Background(), "daemon", smbTestPassword); err == nil {
				t.Fatal("SetPassword succeeded")
			}
			if n := len(cmd.Calls()); n != 0 {
				t.Errorf("smbpasswd ran %d times though the system account could not be made", n)
			}
		})
	}
}

func TestSmbpasswdAccounts_DeleteRemovesTheSambaEntryBeforeTheSystemAccount(t *testing.T) {
	var removedAtSmbpasswd []string
	var unix *FakeUnixAccounts
	acc, unix, cmd := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		removedAtSmbpasswd = unix.Events()
		return CommandResult{}, nil
	})
	if err := acc.Delete(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if len(removedAtSmbpasswd) != 0 {
		t.Errorf("the system account was removed before smbpasswd -x ran: %v", removedAtSmbpasswd)
	}
	if !reflect.DeepEqual(unix.Events(), []string{"remove:alice"}) {
		t.Errorf("events = %v, want the system account removed after", unix.Events())
	}
	calls := cmd.Calls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"-x", "alice"}) {
		t.Errorf("commands = %+v, want smbpasswd -x alice", calls)
	}
}

func TestSmbpasswdAccounts_DeleteOfAnAccountWithNoSambaEntryStillRemovesTheSystemAccount(t *testing.T) {
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: "Failed to find entry for user alice."}, nil
	})
	if err := acc.Delete(context.Background(), "alice"); err != nil {
		t.Fatalf("Delete = %v, want an absent Samba entry to be no error", err)
	}
	if !reflect.DeepEqual(unix.Events(), []string{"remove:alice"}) {
		t.Errorf("events = %v", unix.Events())
	}
}

func TestSmbpasswdAccounts_DeleteKeepsTheSystemAccountWhenTheSambaEntryCannotBeRemoved(t *testing.T) {
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: "passdb locked"}, nil
	})
	if err := acc.Delete(context.Background(), "alice"); err == nil || !strings.Contains(err.Error(), "passdb locked") {
		t.Fatalf("Delete = %v, want smbpasswd's failure", err)
	}
	if len(unix.Events()) != 0 {
		t.Errorf("the system account was touched after smbpasswd -x failed: %v", unix.Events())
	}
}

func TestSmbpasswdAccounts_DeleteFailingAtTheSystemAccountLeavesNoSambaLogin(t *testing.T) {
	passdb := map[string]bool{"alice": true}
	acc, unix, _ := newSmbpasswdForTest(func(c FakeCall) (CommandResult, error) {
		if c.Name == "smbpasswd" && c.Args[0] == "-x" {
			delete(passdb, c.Args[1])
		}
		return CommandResult{}, nil
	})
	unix.FailRemove(errors.New("userdel failed"))
	if err := acc.Delete(context.Background(), "alice"); err == nil {
		t.Fatal("Delete succeeded though the system account could not be removed")
	}
	if passdb["alice"] {
		t.Error("a Samba entry survived a delete whose system-account step failed: the user could still log in")
	}
}

func TestSmbpasswdAccounts_DeleteSkipsAnInvalidName(t *testing.T) {
	acc, unix, cmd := newSmbpasswdForTest(nil)
	if err := acc.Delete(context.Background(), "-x"); err != nil {
		t.Fatal(err)
	}
	if len(cmd.Calls()) != 0 || len(unix.Events()) != 0 {
		t.Errorf("commands ran for an invalid name: %+v %v", cmd.Calls(), unix.Events())
	}
}

func TestNewSmbpasswdAccountsProvisionsSystemAccountsWithRealCommands(t *testing.T) {
	acc := NewSmbpasswdAccounts()
	if _, ok := acc.Unix.(CommandUnixAccounts); !ok {
		t.Errorf("the production SambaAccounts' UnixAccounts is %T, want CommandUnixAccounts", acc.Unix)
	}
	if _, ok := acc.Cmd.(ExecCommander); !ok {
		t.Errorf("the production SambaAccounts' Commander is %T, want ExecCommander", acc.Cmd)
	}
}
