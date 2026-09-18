//go:build linux

package parity

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeFsid and fakeHandle are arbitrary, fixed test fixtures standing in
// for the kernel's own __kernel_fsid_t and file_handle — this file never
// touches a real fanotify fd; it only exercises decodeFanotifyBatch
// against hand-built byte buffers shaped like fanotify_event_metadata +
// fanotify_event_info_fid records (linux/fanotify.h), matching spike
// S7's own on-wire layout (doc 08 §7).
var (
	fakeFsid   = []byte{1, 2, 3, 4, 5, 6, 7, 8}
	fakeHandle = []byte{0xAA, 0xBB, 0xCC, 0xDD}
)

// buildDfidNameEvent encodes one fanotify_event_metadata record carrying
// exactly one FAN_EVENT_INFO_TYPE_DFID_NAME (or OLD/NEW variant) info
// record, byte-for-byte matching what the kernel would emit.
func buildDfidNameEvent(mask uint64, infoType byte, name string) []byte {
	nameBytes := append([]byte(name), 0) // NUL-terminated, as fanotify reports it
	body := make([]byte, 0, fanotifyFsidLen+8+len(fakeHandle)+len(nameBytes))
	body = append(body, fakeFsid...)
	handleBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(handleBytes, uint32(len(fakeHandle)))
	body = append(body, handleBytes...)
	handleType := make([]byte, 4)
	binary.LittleEndian.PutUint32(handleType, 1)
	body = append(body, handleType...)
	body = append(body, fakeHandle...)
	body = append(body, nameBytes...)

	infoRecLen := fanotifyInfoHeaderLen + len(body)
	infoRec := make([]byte, infoRecLen)
	infoRec[0] = infoType
	binary.LittleEndian.PutUint16(infoRec[2:4], uint16(infoRecLen))
	copy(infoRec[fanotifyInfoHeaderLen:], body)

	eventLen := fanotifyMetadataLen + len(infoRec)
	event := make([]byte, eventLen)
	binary.LittleEndian.PutUint32(event[0:4], uint32(eventLen))
	event[4] = 3 // vers, arbitrary — decodeFanotifyBatch never reads it
	binary.LittleEndian.PutUint16(event[6:8], uint16(fanotifyMetadataLen))
	binary.LittleEndian.PutUint64(event[8:16], mask)
	noFd := int32(unix.FAN_NOFD)
	binary.LittleEndian.PutUint32(event[16:20], uint32(noFd))
	copy(event[fanotifyMetadataLen:], infoRec)
	return event
}

func buildOverflowEvent() []byte {
	event := make([]byte, fanotifyMetadataLen)
	binary.LittleEndian.PutUint32(event[0:4], uint32(fanotifyMetadataLen))
	binary.LittleEndian.PutUint16(event[6:8], uint16(fanotifyMetadataLen))
	binary.LittleEndian.PutUint64(event[8:16], unix.FAN_Q_OVERFLOW)
	noFd := int32(unix.FAN_NOFD)
	binary.LittleEndian.PutUint32(event[16:20], uint32(noFd))
	return event
}

func noResolve(fsid []byte, handleType int32, handle []byte, name string) string { return "" }

func TestDecodeFanotifyBatch_Create(t *testing.T) {
	data := buildDfidNameEvent(unix.FAN_CREATE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "a.txt")
	events := decodeFanotifyBatch(data, noResolve)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].Overflow {
		t.Fatal("got Overflow=true, want a Change event")
	}
	c := events[0].Change
	if c.Kind != ChangeCreate {
		t.Fatalf("Kind = %v, want ChangeCreate", c.Kind)
	}
	if c.Name != "a.txt" {
		t.Fatalf("Name = %q, want %q", c.Name, "a.txt")
	}
	if c.ID == "" {
		t.Fatal("ID is empty")
	}
}

func TestDecodeFanotifyBatch_KindPriority(t *testing.T) {
	// CLOSE_WRITE commonly coalesces with MODIFY on a real write+close —
	// DELETE must win over CREATE (a create-then-unlink-in-window nets
	// out as a deletion, the more consequential fact for the threshold
	// guard's own accounting, doc 02 §2), and CLOSE_WRITE must win over a
	// bare MODIFY (it is the point the write is actually durable-ish).
	cases := []struct {
		mask uint64
		want ChangeKind
	}{
		{unix.FAN_DELETE | unix.FAN_CREATE, ChangeDelete},
		{unix.FAN_MODIFY | unix.FAN_CLOSE_WRITE, ChangeCloseWrite},
		{unix.FAN_ATTRIB, ChangeAttrib},
	}
	for _, c := range cases {
		data := buildDfidNameEvent(c.mask, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "f")
		events := decodeFanotifyBatch(data, noResolve)
		if len(events) != 1 || events[0].Change.Kind != c.want {
			t.Fatalf("mask %#x: got %+v, want Kind=%v", c.mask, events, c.want)
		}
	}
}

func TestDecodeFanotifyBatch_Rename(t *testing.T) {
	old := buildDfidNameEvent(unix.FAN_RENAME, unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME, "old.txt")
	events := decodeFanotifyBatch(old, noResolve)
	if len(events) != 1 || events[0].Change.Kind != ChangeRenameFrom || events[0].Change.Name != "old.txt" {
		t.Fatalf("OLD_DFID_NAME: got %+v", events)
	}

	newer := buildDfidNameEvent(unix.FAN_RENAME, unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME, "new.txt")
	events = decodeFanotifyBatch(newer, noResolve)
	if len(events) != 1 || events[0].Change.Kind != ChangeRenameTo || events[0].Change.Name != "new.txt" {
		t.Fatalf("NEW_DFID_NAME: got %+v", events)
	}
}

func TestDecodeFanotifyBatch_Overflow(t *testing.T) {
	events := decodeFanotifyBatch(buildOverflowEvent(), noResolve)
	if len(events) != 1 || !events[0].Overflow {
		t.Fatalf("got %+v, want a single Overflow event", events)
	}
}

func TestDecodeFanotifyBatch_MultipleEventsInOneRead(t *testing.T) {
	var buf []byte
	buf = append(buf, buildDfidNameEvent(unix.FAN_CREATE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "a.txt")...)
	buf = append(buf, buildOverflowEvent()...)
	buf = append(buf, buildDfidNameEvent(unix.FAN_DELETE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "b.txt")...)

	events := decodeFanotifyBatch(buf, noResolve)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Change.Kind != ChangeCreate || events[0].Change.Name != "a.txt" {
		t.Fatalf("event 0 = %+v", events[0])
	}
	if !events[1].Overflow {
		t.Fatalf("event 1 = %+v, want Overflow", events[1])
	}
	if events[2].Change.Kind != ChangeDelete || events[2].Change.Name != "b.txt" {
		t.Fatalf("event 2 = %+v", events[2])
	}
}

func TestDecodeFanotifyBatch_SameDirAndNameProduceSameID(t *testing.T) {
	first := decodeFanotifyBatch(buildDfidNameEvent(unix.FAN_CREATE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "a.txt"), noResolve)
	second := decodeFanotifyBatch(buildDfidNameEvent(unix.FAN_CLOSE_WRITE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "a.txt"), noResolve)
	if first[0].Change.ID != second[0].Change.ID {
		t.Fatalf("IDs differ for the same directory handle + name: %q vs %q", first[0].Change.ID, second[0].Change.ID)
	}
}

func TestDecodeFanotifyBatch_TruncatedTrailingEventIsDropped(t *testing.T) {
	full := buildDfidNameEvent(unix.FAN_CREATE, unix.FAN_EVENT_INFO_TYPE_DFID_NAME, "a.txt")
	truncated := append(append([]byte(nil), full...), full[:fanotifyMetadataLen-1]...)

	events := decodeFanotifyBatch(truncated, noResolve)
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly the one complete event (truncated trailing bytes dropped): %+v", len(events), events)
	}
}

func TestFanotifyEventMask_NoTimerOnlyFileEvents(t *testing.T) {
	// This is a documentation-level guard, not a behavioural one: the
	// mask must never include anything that isn't a genuine
	// kernel-pushed filesystem event (CLAUDE.md: "nothing on a timer
	// walks a data disk" — the journal has nothing to walk in the first
	// place, but this at least confirms the mask itself carries no
	// polling-flavoured bit like FAN_OPEN/FAN_ACCESS that would turn
	// every read into a counted "change").
	disallowed := uint64(unix.FAN_OPEN | unix.FAN_ACCESS | unix.FAN_OPEN_EXEC)
	if uint64(fanotifyEventMask)&disallowed != 0 {
		t.Fatalf("fanotifyEventMask %#x includes a non-change event bit", fanotifyEventMask)
	}
}
