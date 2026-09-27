package disk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"
	"time"
)

// blockingUntilCancelledRunner is a Runner that never returns on its own
// — it blocks until ctx is done — for proving ProbeBlank's own bound
// (#398 finding 2) actually cuts a hung blkid exec off, rather than only
// claiming to.
type blockingUntilCancelledRunner struct{}

func (blockingUntilCancelledRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// fakeExitError scripts a Run error with a given exit code, without
// spawning a real process just to obtain one — matched by ProbeBlank's
// own exitCoder interface, the same pattern internal/parity's own tests
// use for snapraid's exit codes.
type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return "exit status" }
func (e *fakeExitError) ExitCode() int { return e.code }

func TestLinuxBlankProber_NoSignatureReportsBlank(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: blkidNoSignature})
	readback := NewFakeBlankReadback()
	readback.ScriptOK("/dev/sdb")
	p := LinuxBlankProber{Exec: r, Readback: readback}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("ProbeBlank: %v", err)
	}
	if !blank {
		t.Fatal("ProbeBlank = false, want true — blkid -p exit 2 plus a clean readback is a positive 'no signature' result")
	}
	if opened := readback.Opened(); len(opened) != 1 || opened[0] != "/dev/sdb" {
		t.Fatalf("Opened() = %v, want exactly one readback of /dev/sdb", opened)
	}
}

// TestLinuxBlankProber_NoSignatureButUnreadableRefuses is finding 1's own
// regression: blkid -p's exit 2 also covers "impossible to gather any
// information about the device" (blkid(8)) — a disk with read errors in
// its first sectors exits 2 the same as a genuinely blank one. Without
// the readback below, ProbeBlank would report this disk blank and let
// FormatForAddition overwrite data the read errors kept blkid from ever
// actually inspecting.
func TestLinuxBlankProber_NoSignatureButUnreadableRefuses(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: blkidNoSignature})
	readback := NewFakeBlankReadback()
	readback.ScriptError("/dev/sdb", errors.New("input/output error"))
	p := LinuxBlankProber{Exec: r, Readback: readback}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err == nil {
		t.Fatal("ProbeBlank: got nil error, want one — blkid's own exit 2 is never trusted without a clean readback")
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false — the readback failed, this disk was never confirmed genuinely blank")
	}
}

// TestLinuxBlankProber_NoSignatureButShortReadRefuses proves a readback
// that opens the device but returns fewer bytes than asked for is
// treated exactly like any other readback failure, never as "close
// enough to blank".
func TestLinuxBlankProber_NoSignatureButShortReadRefuses(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: blkidNoSignature})
	readback := NewFakeBlankReadback()
	readback.ScriptError("/dev/sdb", errors.New("short read"))
	p := LinuxBlankProber{Exec: r, Readback: readback}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err == nil {
		t.Fatal("ProbeBlank: got nil error, want one for a short read")
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false on a short read")
	}
}

// TestLinuxBlankProber_FoundSignatureRefuses is the partition-table half
// of #398's own data-loss test: a disk carrying a filesystem or a
// partition table (blkid -p's own exit 0, a successful probe) must never
// be read as blank.
func TestLinuxBlankProber_FoundSignatureRefuses(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, []byte("PTTYPE=gpt\n"), nil)
	p := LinuxBlankProber{Exec: r}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("ProbeBlank: %v", err)
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false — blkid -p found a signature (a partition table), this disk is not blank")
	}
}

func TestLinuxBlankProber_AmbiguousRefusesWithError(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: blkidAmbiguous})
	p := LinuxBlankProber{Exec: r}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err == nil {
		t.Fatal("ProbeBlank: got nil error, want one — an ambiguous low-level result is never a positive 'blank' answer")
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false on an ambiguous result")
	}
}

func TestLinuxBlankProber_OtherErrorRefusesWithError(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, errors.New("blkid: not found"))
	p := LinuxBlankProber{Exec: r}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err == nil {
		t.Fatal("ProbeBlank: got nil error, want one — a tool failure with no exit code is never a positive 'blank' answer")
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false on a tool failure")
	}
}

func TestLinuxBlankProber_UsageErrorRefusesWithError(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: 4})
	p := LinuxBlankProber{Exec: r}

	blank, err := p.ProbeBlank(context.Background(), "/dev/sdb")
	if err == nil {
		t.Fatal("ProbeBlank: got nil error, want one for blkid's own usage/other-error exit code")
	}
	if blank {
		t.Fatal("ProbeBlank = true, want false on a usage error")
	}
}

func TestLinuxBlankProber_NeverExecsAShell(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-p", "-o", "export", "/dev/sdb"}, nil, &fakeExitError{code: blkidNoSignature})
	readback := NewFakeBlankReadback()
	readback.ScriptOK("/dev/sdb")
	p := LinuxBlankProber{Exec: r, Readback: readback}

	if _, err := p.ProbeBlank(context.Background(), "/dev/sdb"); err != nil {
		t.Fatalf("ProbeBlank: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls() = %d calls, want exactly 1", len(calls))
	}
	if calls[0].Name != "blkid" {
		t.Fatalf("Calls()[0].Name = %q, want blkid — never a shell", calls[0].Name)
	}
}

func TestLinuxBlankProber_CancelledContextRefusesBeforeExec(t *testing.T) {
	r := NewFakeRunner()
	p := LinuxBlankProber{Exec: r}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.ProbeBlank(ctx, "/dev/sdb"); err == nil {
		t.Fatal("ProbeBlank: got nil error for a cancelled context")
	}
	if len(r.Calls()) != 0 {
		t.Fatalf("Calls() = %d calls, want 0 — a cancelled context must never reach exec", len(r.Calls()))
	}
}

// TestLinuxBlankProber_BoundsAHungExec is finding 2's own regression:
// without a context.WithTimeout inside ProbeBlank itself, a mount_failed
// disk whose blkid exec hangs (a wedged controller, not simply "no
// filesystem") blocks planDiskReplace/replaceDisk — and the request that
// reaches them — indefinitely, since neither the request context nor the
// job context this runs on otherwise bounds it.
func TestLinuxBlankProber_BoundsAHungExec(t *testing.T) {
	p := LinuxBlankProber{Exec: blockingUntilCancelledRunner{}, Timeout: 50 * time.Millisecond}

	done := make(chan struct{})
	var err error
	start := time.Now()
	go func() {
		_, err = p.ProbeBlank(context.Background(), "/dev/sdb")
		close(done)
	}()

	select {
	case <-done:
		if err == nil {
			t.Fatal("ProbeBlank with a blkid exec that never returns = nil error, want one")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ProbeBlank took %v, want it bounded near Timeout (%v)", elapsed, p.Timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeBlank did not return within 5s of a blkid exec that never returns on its own")
	}
}

// TestOSBlankReadback_ReadsCleanlyOnAWholeFile proves the real readback
// against an ordinary file standing in for a device: a file shorter than
// one chunk is read whole with no error.
func TestOSBlankReadback_ReadsCleanlyOnAWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := (osBlankReadback{}).Readback(context.Background(), path); err != nil {
		t.Fatalf("Readback: %v", err)
	}
}

// TestOSBlankReadback_ReadsFirstAndLastChunkOfALargerFile proves the
// real readback covers both ends of a device larger than one chunk, not
// only its very start.
func TestOSBlankReadback_ReadsFirstAndLastChunkOfALargerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank")
	if err := os.WriteFile(path, make([]byte, 3*blankReadbackChunk), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := (osBlankReadback{}).Readback(context.Background(), path); err != nil {
		t.Fatalf("Readback: %v", err)
	}
}

// TestOSBlankReadback_MissingPathRefuses proves the real readback treats
// a device it cannot even open exactly like any other failure — refuse.
func TestOSBlankReadback_MissingPathRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	if err := (osBlankReadback{}).Readback(context.Background(), path); err == nil {
		t.Fatal("Readback: got nil error for a nonexistent path, want one")
	}
}

// TestReadExactly_ReaderErrorIsReturned is finding 4's own regression:
// readExactly is the guard behind both of readbackDevice's own reads, and
// it is the only thing standing between a device with read errors in its
// first or last sectors and ProbeBlank reporting it blank — every other
// test in this file exercises that guard only through FakeBlankReadback,
// an interface fake that never calls readExactly (or io.ReadFull) at all,
// so changing readExactly to ignore io.ReadFull's own error would pass
// every one of them. iotest.ErrReader stands in for a real device whose
// first read syscall returns an I/O error rather than any data.
func TestReadExactly_ReaderErrorIsReturned(t *testing.T) {
	wantErr := errors.New("input/output error")
	if err := readExactly(iotest.ErrReader(wantErr), 10); err == nil {
		t.Fatal("readExactly: got nil error, want one for a reader that errors before returning any bytes")
	} else if !errors.Is(err, wantErr) {
		t.Fatalf("readExactly: got %v, want an error wrapping %v", err, wantErr)
	}
}

// TestReadExactly_ShortReadIsReturnedAsAnError is finding 4's other half:
// a reader that returns fewer bytes than requested and then EOF — no
// error of its own at all, exactly what a single short read() syscall
// against a real block device looks like — must still refuse, never be
// treated as "close enough to confirm the device reads cleanly".
func TestReadExactly_ShortReadIsReturnedAsAnError(t *testing.T) {
	if err := readExactly(bytes.NewReader([]byte("short")), 10); err == nil {
		t.Fatal("readExactly: got nil error, want one for a reader with fewer bytes than requested")
	}
}
