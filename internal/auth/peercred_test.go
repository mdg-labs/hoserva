package auth

import "testing"

func TestOSGroupLookupRootAlwaysMember(t *testing.T) {
	ok, err := OSGroupLookup{}.IsMember(PeerCredential{UID: 0, GID: 12345}, "a-group-that-does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error for uid 0: %v", err)
	}
	if !ok {
		t.Error("uid 0 must always be treated as a member (Q44)")
	}
}

func TestOSGroupLookupMissingGroupDegradesCleanly(t *testing.T) {
	_, err := OSGroupLookup{}.IsMember(PeerCredential{UID: 1000, GID: 1000}, "hoserva-group-that-should-not-exist-in-tests")
	if err != ErrGroupNotFound {
		t.Errorf("expected ErrGroupNotFound for a missing group, got %v", err)
	}
}

func TestFakeGroupLookupRootIsMember(t *testing.T) {
	f := &FakeGroupLookup{Group: "hoserva", Exists: true, Members: map[uint32]bool{2000: true}}
	ok, err := f.IsMember(PeerCredential{UID: 0, GID: 9999}, "hoserva")
	if err != nil || !ok {
		t.Errorf("uid 0 must be a member: ok=%v err=%v", ok, err)
	}
}

func TestFakeGroupLookupGroupMember(t *testing.T) {
	f := &FakeGroupLookup{Group: "hoserva", Exists: true, Members: map[uint32]bool{2000: true}}
	ok, err := f.IsMember(PeerCredential{UID: 1000, GID: 2000}, "hoserva")
	if err != nil || !ok {
		t.Errorf("gid 2000 must be a member: ok=%v err=%v", ok, err)
	}
}

func TestFakeGroupLookupStranger(t *testing.T) {
	f := &FakeGroupLookup{Group: "hoserva", Exists: true, Members: map[uint32]bool{2000: true}}
	ok, err := f.IsMember(PeerCredential{UID: 1000, GID: 3000}, "hoserva")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("a uid/gid not in Members must not be a member")
	}
}

func TestFakeGroupLookupGroupDoesNotExist(t *testing.T) {
	f := &FakeGroupLookup{Group: "hoserva", Exists: false}
	ok, err := f.IsMember(PeerCredential{UID: 1000, GID: 2000}, "hoserva")
	if ok {
		t.Error("a non-existent group must never grant membership")
	}
	if err != ErrGroupNotFound {
		t.Errorf("expected ErrGroupNotFound, got %v", err)
	}
}
