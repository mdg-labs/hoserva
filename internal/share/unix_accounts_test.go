package share

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// passwdScript answers getent from entries (name -> passwd line) and every
// other command with exit 0, recording nothing itself: the FakeCommander does.
func passwdScript(entries map[string]string) func(FakeCall) (CommandResult, error) {
	return func(c FakeCall) (CommandResult, error) {
		if c.Name != "getent" {
			return CommandResult{}, nil
		}
		name := c.Args[len(c.Args)-1]
		if line, ok := entries[name]; ok {
			return CommandResult{Stdout: line + "\n"}, nil
		}
		return CommandResult{ExitCode: 2}, nil
	}
}

func commandsNamed(calls []FakeCall, name string) []FakeCall {
	var out []FakeCall
	for _, c := range calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func TestCommandUnixAccounts_EnsureCreatesTheAccountWithTheModelsArgv(t *testing.T) {
	cmd := &FakeCommander{Script: passwdScript(nil)}
	created, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), "alice")
	if err != nil || !created {
		t.Fatalf("Ensure = (%v, %v), want (true, nil)", created, err)
	}
	adds := commandsNamed(cmd.Calls(), "useradd")
	if len(adds) != 1 {
		t.Fatalf("useradd ran %d times, want once: %+v", len(adds), cmd.Calls())
	}
	want := []string{
		"--no-create-home", "--no-user-group", "--gid", "100", "--shell", "/usr/sbin/nologin",
		"--comment", "Hoserva share user",
		"-K", "UID_MIN=30000", "-K", "UID_MAX=39999",
		"--", "alice",
	}
	if !reflect.DeepEqual(adds[0].Args, want) {
		t.Errorf("useradd argv = %q, want %q", adds[0].Args, want)
	}
	for _, a := range adds[0].Args {
		if a == "-p" || a == "--password" {
			t.Errorf("useradd was given a password option %q: the account must stay locked", a)
		}
	}
}

func TestCommandUnixAccounts_EnsureLeavesAManagedAccountAlone(t *testing.T) {
	cmd := &FakeCommander{Script: passwdScript(map[string]string{
		"alice": "alice:x:30001:100:Hoserva share user:/home/alice:/usr/sbin/nologin",
	})}
	created, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), "alice")
	if err != nil || created {
		t.Fatalf("Ensure = (%v, %v), want (false, nil)", created, err)
	}
	if n := len(commandsNamed(cmd.Calls(), "useradd")); n != 0 {
		t.Errorf("useradd ran %d times for an account that already exists", n)
	}
}

func TestCommandUnixAccounts_EnsureRefusesAccountsHoservaDidNotCreate(t *testing.T) {
	for name, line := range map[string]string{
		"root":   "root:x:0:0:root:/root:/bin/bash",
		"daemon": "daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"bob":    "bob:x:1000:1000::/home/bob:/bin/bash",
		"carol":  "carol:x:30005:100::/home/carol:/bin/bash",
		"dave":   "dave:x:40000:100::/nonexistent:/usr/sbin/nologin",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &FakeCommander{Script: passwdScript(map[string]string{name: line})}
			_, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), name)
			if !errors.Is(err, ErrAccountNameTaken) {
				t.Fatalf("Ensure(%s) = %v, want ErrAccountNameTaken", name, err)
			}
			if n := len(commandsNamed(cmd.Calls(), "useradd")); n != 0 {
				t.Errorf("useradd ran for %s", name)
			}
		})
	}
}

func TestCommandUnixAccounts_EnsureRefusesAnInvalidNameBeforeAnyCommand(t *testing.T) {
	for _, name := range []string{"", "Alice", "-oops", "a b", "a;rm -rf", "root\n", "1abc", "a:b", strings.Repeat("a", 65)} {
		cmd := &FakeCommander{}
		_, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), name)
		if !errors.Is(err, ErrInvalidAccountName) {
			t.Errorf("Ensure(%q) = %v, want ErrInvalidAccountName", name, err)
		}
		if n := len(cmd.Calls()); n != 0 {
			t.Errorf("Ensure(%q) ran %d commands before refusing", name, n)
		}
	}
}

func TestCommandUnixAccounts_EnsureReportsAFailedUseradd(t *testing.T) {
	cmd := &FakeCommander{Script: func(c FakeCall) (CommandResult, error) {
		if c.Name == "useradd" {
			return CommandResult{ExitCode: 4, Stderr: "UID range exhausted"}, nil
		}
		return CommandResult{ExitCode: 2}, nil
	}}
	created, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), "alice")
	if err == nil || created || !strings.Contains(err.Error(), "UID range exhausted") {
		t.Fatalf("Ensure = (%v, %v), want a failure naming useradd's message", created, err)
	}
}

func TestCommandUnixAccounts_EnsureAcceptsAManagedAccountCreatedConcurrently(t *testing.T) {
	lookups := 0
	cmd := &FakeCommander{Script: func(c FakeCall) (CommandResult, error) {
		switch c.Name {
		case "getent":
			lookups++
			if lookups == 1 {
				return CommandResult{ExitCode: 2}, nil
			}
			return CommandResult{Stdout: "alice:x:30001:100::/home/alice:/usr/sbin/nologin\n"}, nil
		case "useradd":
			return CommandResult{ExitCode: 9}, nil
		}
		return CommandResult{}, nil
	}}
	created, err := CommandUnixAccounts{Cmd: cmd}.Ensure(context.Background(), "alice")
	if err != nil || created {
		t.Fatalf("Ensure = (%v, %v), want (false, nil)", created, err)
	}
}

func TestCommandUnixAccounts_RemoveDeletesOnlyAManagedAccount(t *testing.T) {
	entries := map[string]string{
		"alice":  "alice:x:30001:100::/home/alice:/usr/sbin/nologin",
		"daemon": "daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"bob":    "bob:x:1000:1000::/home/bob:/bin/bash",
	}
	for name, wantDel := range map[string]bool{"alice": true, "daemon": false, "bob": false, "ghost": false, "Not Valid": false} {
		cmd := &FakeCommander{Script: passwdScript(entries)}
		if err := (CommandUnixAccounts{Cmd: cmd}).Remove(context.Background(), name); err != nil {
			t.Fatalf("Remove(%q) = %v", name, err)
		}
		dels := commandsNamed(cmd.Calls(), "userdel")
		if (len(dels) == 1) != wantDel || len(dels) > 1 {
			t.Errorf("Remove(%q) ran userdel %d times, want deleted=%v", name, len(dels), wantDel)
		}
		if wantDel && !reflect.DeepEqual(dels[0].Args, []string{"--", "alice"}) {
			t.Errorf("userdel argv = %q", dels[0].Args)
		}
	}
}

func TestCommandUnixAccounts_RemoveReportsAFailedUserdel(t *testing.T) {
	cmd := &FakeCommander{Script: func(c FakeCall) (CommandResult, error) {
		if c.Name == "userdel" {
			return CommandResult{ExitCode: 8, Stderr: "user alice is currently used by process 1"}, nil
		}
		return CommandResult{Stdout: "alice:x:30001:100::/home/alice:/usr/sbin/nologin\n"}, nil
	}}
	if err := (CommandUnixAccounts{Cmd: cmd}).Remove(context.Background(), "alice"); err == nil || !strings.Contains(err.Error(), "currently used") {
		t.Fatalf("Remove = %v, want userdel's failure", err)
	}
}
