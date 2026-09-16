// journal is spike S7's throwaway fanotify listener (issue #8, doc 02 §2,
// Q13). It places one FAN_MARK_FILESYSTEM mark on one data-disk mountpoint
// and prints one TSV line per changed-path event to -out, so the spike's
// shell scripts can compare a distinct-path count against `snapraid diff`
// without any JSON tooling.
//
// Standard library only — CLAUDE.md forbids touching go.mod/go.sum for a
// spike, so this hand-declares the fanotify syscalls, flags and on-wire
// structs that golang.org/x/sys/unix would otherwise provide. amd64/linux
// only: SYS_FANOTIFY_INIT and SYS_FANOTIFY_MARK below are the x86-64
// syscall table numbers (arch/x86/entry/syscalls/syscall_64.tbl); this is
// a spike experiment, not the production journal, and the constraint is
// stated in spikes/s7/README.md rather than generalised.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sysFanotifyInit = 300
	sysFanotifyMark = 301

	fanClassNotif   = 0x00000000
	fanCloexec      = 0x00000001
	fanNonblock     = 0x00000002
	fanReportDirFid = 0x00000400
	fanReportName   = 0x00000800
	fanReportDfidNm = fanReportDirFid | fanReportName
	fanUnlimitedQue = 0x00000010

	fanMarkAdd        = 0x00000001
	fanMarkFilesystem = 0x00000100

	fanCreate     = 0x00000100
	fanDelete     = 0x00000200
	fanModify     = 0x00000002
	fanAttrib     = 0x00000004
	fanCloseWrite = 0x00000008
	fanRename     = 0x10000000
	fanQOverflow  = 0x00004000

	fanEventMask = fanCreate | fanDelete | fanModify | fanAttrib | fanCloseWrite | fanRename

	fanEventInfoTypeDfidName    = 2
	fanEventInfoTypeOldDfidName = 10
	fanEventInfoTypeNewDfidName = 12

	fanNoFd = -1

	atFdcwd = -100
)

// struct fanotify_event_metadata (linux/fanotify.h) — 24 bytes, no padding:
// event_len u32, vers u8, reserved u8, metadata_len u16, mask u64(aligned),
// fd s32, pid s32.
const metadataLen = 24

// struct fanotify_event_info_header — 4 bytes: info_type u8, pad u8, len u16.
const infoHeaderLen = 4

// __kernel_fsid_t is 2×s32 = 8 bytes.
const fsidLen = 8

func fanotifyInit(flags, eventFFlags uint) (int, error) {
	fd, _, errno := syscall.Syscall(sysFanotifyInit, uintptr(flags), uintptr(eventFFlags), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func fanotifyMark(fd int, flags uint, mask uint64, dirfd int, path string) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(sysFanotifyMark, uintptr(fd), uintptr(flags), uintptr(mask), uintptr(dirfd), uintptr(unsafe.Pointer(p)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

type record struct {
	infoType byte
	fid      string // hex(fsid) + "-" + hex(handle_type + f_handle) — our opaque per-disk identity key, since open_by_handle_at is unavailable in this lab (no CAP_DAC_READ_SEARCH)
	name     string
}

// parseInfoRecords walks every FAN_EVENT_INFO_* record following one
// event's fixed metadata, from data[off:eventLen].
func parseInfoRecords(data []byte, off, eventLen int) []record {
	var out []record
	for off+infoHeaderLen <= eventLen {
		infoType := data[off]
		recLen := int(binary.LittleEndian.Uint16(data[off+2 : off+4]))
		if recLen < infoHeaderLen || off+recLen > eventLen {
			break // malformed/truncated — stop rather than read out of bounds
		}
		switch infoType {
		case fanEventInfoTypeDfidName, fanEventInfoTypeOldDfidName, fanEventInfoTypeNewDfidName:
			body := data[off+infoHeaderLen : off+recLen]
			if len(body) >= fsidLen+8 {
				fsid := body[:fsidLen]
				handleBytes := binary.LittleEndian.Uint32(body[fsidLen : fsidLen+4])
				handleType := body[fsidLen+4 : fsidLen+8]
				hEnd := fsidLen + 8 + int(handleBytes)
				if hEnd <= len(body) {
					fHandle := body[fsidLen+8 : hEnd]
					nameBytes := body[hEnd:]
					nul := len(nameBytes)
					for i, b := range nameBytes {
						if b == 0 {
							nul = i
							break
						}
					}
					fid := hex.EncodeToString(fsid) + "-" + hex.EncodeToString(handleType) + hex.EncodeToString(fHandle)
					out = append(out, record{infoType: infoType, fid: fid, name: string(nameBytes[:nul])})
				}
			}
		}
		off += recLen
	}
	return out
}

func kindsForMask(mask uint64) []string {
	var kinds []string
	if mask&fanCreate != 0 {
		kinds = append(kinds, "CREATE")
	}
	if mask&fanDelete != 0 {
		kinds = append(kinds, "DELETE")
	}
	if mask&fanModify != 0 {
		kinds = append(kinds, "MODIFY")
	}
	if mask&fanAttrib != 0 {
		kinds = append(kinds, "ATTRIB")
	}
	if mask&fanCloseWrite != 0 {
		kinds = append(kinds, "CLOSE_WRITE")
	}
	return kinds
}

func main() {
	mark := flag.String("mark", "", "directory (any path on the target filesystem) to place a FAN_MARK_FILESYSTEM mark on")
	out := flag.String("out", "", "TSV output path (default: stdout)")
	unlimited := flag.Bool("unlimited", false, "use FAN_UNLIMITED_QUEUE (queue never overflows)")
	ready := flag.String("ready-file", "", "path to write once the mark is installed and before the read loop starts, so a driver script can synchronise a flood against this listener")
	delay := flag.Duration("delay", 0, "sleep this long after signalling ready, before starting to read events — used to build an undrained backlog for the overflow experiment")
	duration := flag.Duration("duration", 0, "exit automatically after this long (0 = run until a signal)")
	flag.Parse()

	if *mark == "" {
		fmt.Fprintln(os.Stderr, "journal: -mark is required")
		os.Exit(2)
	}

	var w *bufio.Writer
	if *out == "" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "journal: creating -out: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		w = bufio.NewWriter(f)
	}
	// Flushed after every line (not just at exit) — this spike restarts the
	// listener and kills it by PID mid-workload, and a buffered line lost to
	// SIGTERM would misreport what the journal actually captured.
	emit := func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
		w.Flush()
	}

	// FAN_NONBLOCK here, not just on the read side, is what makes
	// os.NewFile below treat this fd as pollable and integrate it with the
	// runtime's netpoller — which is what makes file.Close() from another
	// goroutine actually interrupt a Read() blocked waiting for the next
	// event (confirmed necessary empirically: without it, closing the fd
	// from the signal/duration goroutine left the blocked Read() blocked
	// forever, since os.NewFile only attempts poller integration when the
	// fd is already non-blocking at the time it wraps it).
	initFlags := uint(fanClassNotif | fanCloexec | fanNonblock | fanReportDfidNm)
	if *unlimited {
		initFlags |= fanUnlimitedQue
	}
	fd, err := fanotifyInit(initFlags, uint(syscall.O_RDONLY))
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal: fanotify_init: %v (needs CAP_SYS_ADMIN)\n", err)
		os.Exit(1)
	}

	if err := fanotifyMark(fd, fanMarkAdd|fanMarkFilesystem, uint64(fanEventMask), atFdcwd, *mark); err != nil {
		fmt.Fprintf(os.Stderr, "journal: fanotify_mark(FAN_MARK_FILESYSTEM, %s): %v\n", *mark, err)
		os.Exit(1)
	}

	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "journal: writing -ready-file: %v\n", err)
			os.Exit(1)
		}
	}
	if *delay > 0 {
		time.Sleep(*delay)
	}

	buf := make([]byte, 256*1024)
	file := os.NewFile(uintptr(fd), "fanotify")

	// file.Read below blocks in the read(2) syscall whenever no event is
	// pending — which is most of the time, including the whole gap this
	// spike's restart test deliberately creates. A signal only sets a flag
	// a Go goroutine can see at its own next scheduling point, which never
	// comes while the main goroutine is parked in that blocking read; only
	// closing the fd out from under it actually unblocks the syscall (it
	// returns with an error immediately). sync.Once makes "whichever of
	// SIGTERM/SIGINT/the duration timer fires first" the one that sets the
	// reason and does the closing, so the read loop's own error path always
	// finds a reason already recorded, never races to log two different
	// ones.
	var stopReason string
	var stopOnce sync.Once
	stopNow := func(reason string) {
		stopOnce.Do(func() {
			stopReason = reason
			file.Close()
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		stopNow(fmt.Sprintf("signal=%v", s))
	}()
	if *duration > 0 {
		go func() {
			time.Sleep(*duration)
			stopNow("reason=duration")
		}()
	}

readLoop:
	for {
		n, err := file.Read(buf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			stopOnce.Do(func() { stopReason = fmt.Sprintf("reason=read_error:%v", err) })
			emit("%d\tSTOPPED\t%s\t-\n", time.Now().UnixNano(), stopReason)
			break readLoop
		}
		ts := time.Now().UnixNano()

		off := 0
		for off+metadataLen <= n {
			eventLen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
			if eventLen < metadataLen || off+eventLen > n {
				emit("%d\tSTOPPED\treason=malformed_event\t-\n", ts)
				break readLoop
			}
			mdLen := int(binary.LittleEndian.Uint16(buf[off+6 : off+8]))
			mask := binary.LittleEndian.Uint64(buf[off+8 : off+16])
			evFd := int32(binary.LittleEndian.Uint32(buf[off+16 : off+20]))

			if mask&fanQOverflow != 0 {
				emit("%d\tOVERFLOW\t-\t-\n", ts)
			} else {
				recs := parseInfoRecords(buf[off:off+eventLen], mdLen, eventLen)
				if mask&fanRename != 0 {
					for _, r := range recs {
						switch r.infoType {
						case fanEventInfoTypeOldDfidName:
							emit("%d\tRENAME_OLD\t%s\t%s\n", ts, r.fid, r.name)
						case fanEventInfoTypeNewDfidName:
							emit("%d\tRENAME_NEW\t%s\t%s\n", ts, r.fid, r.name)
						}
					}
				} else {
					kinds := kindsForMask(mask)
					for _, r := range recs {
						if r.infoType != fanEventInfoTypeDfidName {
							continue
						}
						for _, k := range kinds {
							emit("%d\t%s\t%s\t%s\n", ts, k, r.fid, r.name)
						}
					}
				}
			}

			if evFd != fanNoFd {
				syscall.Close(int(evFd))
			}
			off += eventLen
		}
	}
}
