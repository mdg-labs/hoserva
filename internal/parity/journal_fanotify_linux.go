//go:build linux

package parity

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// fanotifyEventMask is the set of events Q13's change journal needs:
// create, delete, a write's content-modifying middle and its close, and
// attribute changes (covers `snapraid touch`, doc 02 §2), plus renames.
// This is the same mask spike S7 validated (doc 08 §7).
const fanotifyEventMask = unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_MODIFY |
	unix.FAN_ATTRIB | unix.FAN_CLOSE_WRITE | unix.FAN_RENAME

// FanotifyWatcher is the real Watcher (doc 02 §2, Q13): one
// FAN_MARK_FILESYSTEM mark per data disk, exactly the mechanism spike S7
// validated against a real workload (doc 08 §7) — event delivery is
// entirely kernel-pushed; every Stream.Next blocks on a real read(2) of
// the fanotify fd, never a timer.
type FanotifyWatcher struct{}

var _ Watcher = FanotifyWatcher{}

// Watch places a FAN_MARK_FILESYSTEM mark on mountpoint. It needs
// CAP_SYS_ADMIN (fanotify_init) to succeed at all.
func (FanotifyWatcher) Watch(ctx context.Context, mountpoint string) (Stream, error) {
	// FAN_NONBLOCK is required for os.NewFile below to integrate the fd
	// with the Go runtime's netpoller — without it, closing the fd from
	// another goroutine does not interrupt an in-flight blocking Read
	// (found and fixed building spike S7's listener, doc 08 §7).
	initFlags := uint(unix.FAN_CLASS_NOTIF | unix.FAN_CLOEXEC | unix.FAN_NONBLOCK | unix.FAN_REPORT_DFID_NAME)
	fd, err := unix.FanotifyInit(initFlags, uint(unix.O_RDONLY))
	if err != nil {
		return nil, fmt.Errorf("parity: journal: fanotify_init (needs CAP_SYS_ADMIN): %w", err)
	}

	if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, uint64(fanotifyEventMask), unix.AT_FDCWD, mountpoint); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("parity: journal: fanotify_mark(FAN_MARK_FILESYSTEM, %s): %w", mountpoint, err)
	}

	// A file handle open_by_handle_at can resolve against, so a name can
	// become a full path when the process has CAP_DAC_READ_SEARCH
	// (unconfirmed in this lab, S7's own residual risk) — failing to
	// open it only degrades path resolution to the (fid, name) fallback
	// below; the mark itself already works without it.
	mountFD, mountErr := unix.Open(mountpoint, unix.O_RDONLY|unix.O_DIRECTORY, 0)

	s := &fanotifyStream{
		file:       os.NewFile(uintptr(fd), "fanotify:"+mountpoint),
		mountFD:    mountFD,
		hasMountFD: mountErr == nil,
		buf:        make([]byte, fanotifyReadBufferSize),
	}

	go func() {
		<-ctx.Done()
		s.closeOnce()
	}()
	return s, nil
}

// fanotifyReadBufferSize is the read(2) buffer Next reuses across every
// call on a given stream — sized to hold a large burst of queued events
// in one read without needing to grow.
const fanotifyReadBufferSize = 256 * 1024

type fanotifyStream struct {
	file       *os.File
	mountFD    int
	hasMountFD bool
	// buf is Next's own read(2) buffer, allocated once in Watch and
	// reused for the life of the stream: only one Next call is ever in
	// flight per stream, and decodeFanotifyBatch copies out everything it
	// keeps (StreamEvent's Path/Name/ID are all independently allocated
	// strings), so reusing it between calls is safe. Allocating a fresh
	// 256KB buffer per call was pure churn on a hot path — every fanotify
	// batch on a busy filesystem re-triggers Next.
	buf []byte

	closeMu sync.Mutex
	closed  bool
}

var _ Stream = (*fanotifyStream)(nil)

func (s *fanotifyStream) Close() error {
	s.closeOnce()
	return nil
}

func (s *fanotifyStream) closeOnce() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.file.Close()
	if s.hasMountFD {
		_ = unix.Close(s.mountFD)
	}
}

// Next blocks on a real read(2) of the fanotify fd and decodes whatever
// batch of events that one syscall returned.
func (s *fanotifyStream) Next() ([]StreamEvent, error) {
	for {
		n, err := s.file.Read(s.buf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return nil, err
		}
		return decodeFanotifyBatch(s.buf[:n], s.resolvePath), nil
	}
}

// resolvePath attempts to turn a directory's file handle plus a reported
// name into an absolute path via open_by_handle_at + reading its own
// /proc/self/fd symlink. It returns "" on any failure (most commonly
// EPERM without CAP_DAC_READ_SEARCH, confirmed the case in the
// loop-device lab by spike S7) — callers always have Name as a fallback.
func (s *fanotifyStream) resolvePath(fsid []byte, handleType int32, handle []byte, name string) string {
	if !s.hasMountFD {
		return ""
	}
	fh := unix.NewFileHandle(handleType, handle)
	fd, err := unix.OpenByHandleAt(s.mountFD, fh, unix.O_PATH)
	if err != nil {
		return ""
	}
	defer func() { _ = unix.Close(fd) }()

	dir, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return ""
	}
	return filepath.Join(dir, name)
}

// fanotify_event_metadata (linux/fanotify.h) is 24 bytes, no padding:
// event_len u32, vers u8, reserved u8, metadata_len u16, mask u64
// (8-byte aligned), fd s32, pid s32.
const fanotifyMetadataLen = 24

// fanotify_event_info_header is 4 bytes: info_type u8, pad u8, len u16.
const fanotifyInfoHeaderLen = 4

// __kernel_fsid_t is 2×s32 = 8 bytes.
const fanotifyFsidLen = 8

// decodeFanotifyBatch parses every fanotify_event_metadata record in
// data — exactly what one read(2) returned — into StreamEvents. resolve
// is called at most once per FID+name record to attempt path resolution;
// it never blocks on anything but the syscalls it itself performs.
func decodeFanotifyBatch(data []byte, resolve func(fsid []byte, handleType int32, handle []byte, name string) string) []StreamEvent {
	var out []StreamEvent
	off := 0
	for off+fanotifyMetadataLen <= len(data) {
		eventLen := int(binary.LittleEndian.Uint32(data[off : off+4]))
		if eventLen < fanotifyMetadataLen || off+eventLen > len(data) {
			break // malformed/truncated trailing record — stop rather than misread past it
		}
		mdLen := int(binary.LittleEndian.Uint16(data[off+6 : off+8]))
		mask := binary.LittleEndian.Uint64(data[off+8 : off+16])
		evFd := int32(binary.LittleEndian.Uint32(data[off+16 : off+20]))

		if mask&unix.FAN_Q_OVERFLOW != 0 {
			out = append(out, StreamEvent{Overflow: true})
		} else {
			for _, rec := range parseFanotifyInfoRecords(data[off:off+eventLen], mdLen, eventLen) {
				kind, ok := changeKindFor(rec.infoType, mask)
				if !ok {
					continue
				}
				out = append(out, StreamEvent{Change: ChangeEvent{
					Kind: kind,
					ID:   rec.id(),
					Name: rec.name,
					Path: resolve(rec.fsid, rec.handleType, rec.handle, rec.name),
				}})
			}
		}

		if evFd != unix.FAN_NOFD {
			_ = unix.Close(int(evFd))
		}
		off += eventLen
	}
	return out
}

func changeKindFor(infoType byte, mask uint64) (ChangeKind, bool) {
	switch infoType {
	case unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME:
		return ChangeRenameFrom, true
	case unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME:
		return ChangeRenameTo, true
	case unix.FAN_EVENT_INFO_TYPE_DFID_NAME:
		switch {
		case mask&unix.FAN_DELETE != 0:
			return ChangeDelete, true
		case mask&unix.FAN_CREATE != 0:
			return ChangeCreate, true
		case mask&unix.FAN_CLOSE_WRITE != 0:
			return ChangeCloseWrite, true
		case mask&unix.FAN_MODIFY != 0:
			return ChangeModify, true
		case mask&unix.FAN_ATTRIB != 0:
			return ChangeAttrib, true
		}
	}
	return 0, false
}

type fanotifyInfoRecord struct {
	infoType   byte
	fsid       []byte
	handleType int32
	handle     []byte
	name       string
}

// id is the opaque (directory-FID, reported-name) identity spike S7
// validated as a distinct-path proxy (doc 08 §7).
func (r fanotifyInfoRecord) id() string {
	return hex.EncodeToString(r.fsid) + "-" + strconv.Itoa(int(r.handleType)) + "-" + hex.EncodeToString(r.handle) + "/" + r.name
}

// parseFanotifyInfoRecords walks every FAN_EVENT_INFO_* record following
// one event's fixed metadata, from data[off:eventLen], keeping only the
// three DFID_NAME variants this journal cares about.
func parseFanotifyInfoRecords(data []byte, off, eventLen int) []fanotifyInfoRecord {
	var out []fanotifyInfoRecord
	for off+fanotifyInfoHeaderLen <= eventLen {
		infoType := data[off]
		recLen := int(binary.LittleEndian.Uint16(data[off+2 : off+4]))
		if recLen < fanotifyInfoHeaderLen || off+recLen > eventLen {
			break
		}
		switch infoType {
		case unix.FAN_EVENT_INFO_TYPE_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME:
			body := data[off+fanotifyInfoHeaderLen : off+recLen]
			if len(body) >= fanotifyFsidLen+8 {
				fsid := body[:fanotifyFsidLen]
				handleBytes := binary.LittleEndian.Uint32(body[fanotifyFsidLen : fanotifyFsidLen+4])
				handleType := int32(binary.LittleEndian.Uint32(body[fanotifyFsidLen+4 : fanotifyFsidLen+8]))
				hEnd := fanotifyFsidLen + 8 + int(handleBytes)
				if hEnd <= len(body) {
					fHandle := body[fanotifyFsidLen+8 : hEnd]
					nameBytes := body[hEnd:]
					nul := len(nameBytes)
					for i, b := range nameBytes {
						if b == 0 {
							nul = i
							break
						}
					}
					out = append(out, fanotifyInfoRecord{
						infoType:   infoType,
						fsid:       append([]byte(nil), fsid...),
						handleType: handleType,
						handle:     append([]byte(nil), fHandle...),
						name:       string(nameBytes[:nul]),
					})
				}
			}
		}
		off += recLen
	}
	return out
}
