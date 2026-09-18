package parity

import (
	"bufio"
	"bytes"
	"context"
	"testing"
)

func TestScanLinesOrCR(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"plain newlines", "a\nb\nc\n", []string{"a", "b", "c"}},
		{"bare CR progress rewrite", "0%, 0 MB          \r100% completed, 1 MB accessed in 0:00    \n", []string{"0%, 0 MB          ", "100% completed, 1 MB accessed in 0:00    "}},
		{"CRLF treated as one break", "a\r\nb\n", []string{"a", "b"}},
		{"no trailing newline", "a\nb", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scanner := bufio.NewScanner(bytes.NewBufferString(c.input))
			scanner.Split(scanLinesOrCR)
			var got []string
			for scanner.Scan() {
				got = append(got, scanner.Text())
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d lines %v, want %d lines %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestCommandRunner_RealProcess(t *testing.T) {
	proc, err := (CommandRunner{}).Start(context.Background(), "printf", "a\\rb\\nc")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var lines []string
	for line := range proc.Lines() {
		lines = append(lines, line)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(lines) != len(want) {
		t.Fatalf("got %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, lines[i], want[i])
		}
	}
}
