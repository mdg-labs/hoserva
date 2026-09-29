package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// deadlineRunner answers lsjson with one file of a given size and records
// the deadline each other call was given.
type deadlineRunner struct {
	name      string
	size      int64
	deadlines map[string]time.Duration
}

func (r *deadlineRunner) Run(ctx context.Context, c RcloneCommand) ([]byte, error) {
	if c.Args[0] == "lsjson" {
		return json.Marshal([]rcloneFile{{Name: r.name, Size: r.size}})
	}
	if dl, ok := ctx.Deadline(); ok {
		r.deadlines[c.Args[0]] = time.Until(dl)
	}
	return nil, nil
}

// A multi-gigabyte appdata archive is given time for its size, not the
// fixed deadline a small config archive fits in.
func TestRcloneTarget_ACopyIsGivenTimeForItsSize(t *testing.T) {
	const gib = int64(1) << 30
	if got := rcloneTransferTimeout(0); got != rcloneCopyTimeout {
		t.Fatalf("timeout for an empty file = %s, want %s", got, rcloneCopyTimeout)
	}
	if got := rcloneTransferTimeout(10 * gib); got < 5*time.Hour {
		t.Fatalf("timeout for 10 GiB = %s, want at least 5h", got)
	}

	r := &deadlineRunner{name: "big.tar.zst", size: 10 * gib, deadlines: map[string]time.Duration{}}
	target := &rcloneTarget{runner: r, dir: "remote:backups"}
	if err := target.fetch(context.Background(), r.name, filepath.Join(t.TempDir(), r.name)); err != nil {
		t.Fatal(err)
	}
	if got := r.deadlines["copyto"]; got < 5*time.Hour {
		t.Fatalf("fetch of 10 GiB was given %s, want at least 5h", got)
	}

	src := filepath.Join(t.TempDir(), "up.tar.zst")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, 10*gib); err != nil {
		t.Fatal(err)
	}
	r.name, r.size = "up.tar.zst", 10*gib
	if err := target.write(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if got := r.deadlines["copy"]; got < 5*time.Hour {
		t.Fatalf("upload of 10 GiB was given %s, want at least 5h", got)
	}
}
