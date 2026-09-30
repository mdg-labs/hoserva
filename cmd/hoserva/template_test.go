package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const templateFixtures = "../../internal/template/testdata/catalog"

func buildHoserva(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hoserva")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runLint runs the built command with no daemon socket anywhere, so it also
// proves the command works offline.
func runLint(t *testing.T, bin string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	var out, errb strings.Builder
	cmd := exec.Command(bin, append([]string{"--socket", filepath.Join(t.TempDir(), "absent.sock"), "template", "lint"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("hoserva template lint did not finish")
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errb.String(), exit
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTemplateLintCommand(t *testing.T) {
	bin := buildHoserva(t)

	t.Run("a valid catalog passes", func(t *testing.T) {
		stdout, stderr, exit := runLint(t, bin, templateFixtures)
		if exit != 0 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
		}
		if !strings.Contains(stdout, "no problems found") {
			t.Errorf("stdout = %q", stdout)
		}
	})

	broken := t.TempDir()
	copyDir(t, templateFixtures, broken)
	file := filepath.Join(broken, "jellyfin", "compose.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, edit := range [][2]string{
		{"  title: Jellyfin\n", ""},
		{"WEBUI_PORT: { kind: port,", "WEBUI_PORT: { kind: number,"},
	} {
		if !strings.Contains(src, edit[0]) {
			t.Fatalf("fixture has no %q", edit[0])
		}
		src = strings.Replace(src, edit[0], edit[1], 1)
	}
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("a malformed block fails with a clear message", func(t *testing.T) {
		stdout, _, exit := runLint(t, bin, broken)
		if exit == 0 {
			t.Fatalf("a malformed template passed lint:\n%s", stdout)
		}
		for _, want := range []string{
			"jellyfin/compose.yaml: line 17: x-hoserva: missing property 'title'",
			"x-hoserva.inputs.WEBUI_PORT.kind: value must be one of",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("--json lists the findings", func(t *testing.T) {
		stdout, _, exit := runLint(t, bin, "--json", broken)
		if exit == 0 {
			t.Fatal("a malformed template passed lint")
		}
		var got []lintFinding
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not the findings array: %v\n%s", err, stdout)
		}
		if len(got) != 2 || got[0].File != "jellyfin/compose.yaml" || got[0].Message == "" {
			t.Errorf("findings = %+v", got)
		}
	})

	t.Run("an unreadable directory is an error", func(t *testing.T) {
		_, stderr, exit := runLint(t, bin, filepath.Join(t.TempDir(), "absent"))
		if exit == 0 || !strings.Contains(stderr, "reading the catalog directory") {
			t.Errorf("exit %d, stderr %q", exit, stderr)
		}
	})

	t.Run("a directory operand is required", func(t *testing.T) {
		if _, _, exit := runLint(t, bin); exit == 0 {
			t.Error("lint without a directory succeeded")
		}
	})
}
