package disk

import "testing"

func TestIdentity_Matches(t *testing.T) {
	cases := []struct {
		name string
		a, b Identity
		want bool
	}{
		{"same WWN", Identity{WWN: "wwn-1", Serial: "s1"}, Identity{WWN: "wwn-1", Serial: "s2"}, true},
		{"different WWN", Identity{WWN: "wwn-1"}, Identity{WWN: "wwn-2"}, false},
		{"same serial, no WWN", Identity{Serial: "s1", WeakIdentity: true}, Identity{Serial: "s1", WeakIdentity: true}, true},
		{"different serial, no WWN", Identity{Serial: "s1"}, Identity{Serial: "s2"}, false},
		{"neither has anything", Identity{WeakIdentity: true}, Identity{WeakIdentity: true}, false},
	}
	for _, c := range cases {
		if got := c.a.Matches(c.b); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func expected(role, wwn string) ExpectedDisk {
	return ExpectedDisk{Identity: Identity{WWN: wwn}, Role: role, MountAt: "/mnt/" + role}
}

// TestStorageGate_SameSerialWrongFilesystemIsNotReady is #388's own
// regression test: a replacement disk that satisfies the identity check
// (Q21: matched by serial/WWN) but carries a different filesystem UUID
// than the slot's recorded one must never be reported ready — reporting
// it ready is exactly what let hoservad's own startup mount call hang
// waiting on a /dev/disk/by-uuid/<uuid> device that never appears, past
// systemd's TimeoutStartSec, restarting forever.
func TestStorageGate_SameSerialWrongFilesystemIsNotReady(t *testing.T) {
	want := ExpectedDisk{
		Identity: Identity{WWN: "wwn-1", FSUUID: "original-uuid"},
		Role:     "disk1",
		MountAt:  "/mnt/disk1",
	}
	g := NewStorageGate([]ExpectedDisk{want})

	check := g.Evaluate([]Identity{{WWN: "wwn-1", FSUUID: "blank-replacement-uuid"}})

	if check.Ready || g.Ready() {
		t.Fatal("Ready with the same serial but a mismatched filesystem UUID, want false")
	}
	if len(check.Missing) != 0 {
		t.Fatalf("Missing = %+v, want empty — the disk is physically present", check.Missing)
	}
	if len(check.WrongFilesystem) != 1 || check.WrongFilesystem[0].Role != "disk1" {
		t.Fatalf("WrongFilesystem = %+v, want exactly disk1", check.WrongFilesystem)
	}
	if len(g.WrongFilesystem()) != 1 {
		t.Fatalf("g.WrongFilesystem() = %+v, want disk1 reported", g.WrongFilesystem())
	}
	if len(g.Missing()) != 0 {
		t.Fatalf("g.Missing() = %+v, want empty — a wrong-filesystem disk is not a missing one", g.Missing())
	}
}

// TestStorageGate_MatchingFilesystemIsReady proves the check above never
// fires for the ordinary case: the same disk, still carrying the
// filesystem UUID it was formatted with.
func TestStorageGate_MatchingFilesystemIsReady(t *testing.T) {
	want := ExpectedDisk{Identity: Identity{WWN: "wwn-1", FSUUID: "uuid-1"}, Role: "disk1", MountAt: "/mnt/disk1"}
	g := NewStorageGate([]ExpectedDisk{want})

	check := g.Evaluate([]Identity{{WWN: "wwn-1", FSUUID: "uuid-1"}})

	if !check.Ready || !g.Ready() {
		t.Fatal("Ready with a matching filesystem UUID, want true")
	}
	if len(check.WrongFilesystem) != 0 {
		t.Fatalf("WrongFilesystem = %+v, want empty", check.WrongFilesystem)
	}
}

// TestStorageGate_UnknownFilesystemIsNotAMismatch proves a slot that has
// never recorded a filesystem UUID (or a present disk this build could not
// read one from) is never reported as a mismatch on that basis alone —
// only ExpectedDisk's own doc comment's "left empty" case, not a real
// disagreement.
func TestStorageGate_UnknownFilesystemIsNotAMismatch(t *testing.T) {
	want := ExpectedDisk{Identity: Identity{WWN: "wwn-1"}, Role: "disk1", MountAt: "/mnt/disk1"}
	g := NewStorageGate([]ExpectedDisk{want})

	check := g.Evaluate([]Identity{{WWN: "wwn-1"}})

	if !check.Ready {
		t.Fatal("Ready with neither side carrying a filesystem UUID, want true")
	}
	if len(check.WrongFilesystem) != 0 {
		t.Fatalf("WrongFilesystem = %+v, want empty", check.WrongFilesystem)
	}
}

func TestStorageGate_ReadyOnlyWhenEveryExpectedDiskIsPresent(t *testing.T) {
	g := NewStorageGate([]ExpectedDisk{expected("disk1", "wwn-1"), expected("disk2", "wwn-2")})

	if g.Ready() {
		t.Fatal("Ready before any Evaluate call, want false")
	}

	check := g.Evaluate([]Identity{{WWN: "wwn-1"}})
	if check.Ready || g.Ready() {
		t.Fatal("Ready with one of two expected disks present, want false")
	}
	if len(check.Missing) != 1 || check.Missing[0].Role != "disk2" {
		t.Fatalf("Missing = %+v, want exactly disk2", check.Missing)
	}

	check = g.Evaluate([]Identity{{WWN: "wwn-1"}, {WWN: "wwn-2"}})
	if !check.Ready || !g.Ready() {
		t.Fatal("Ready with every expected disk present, want true")
	}
	if len(check.Missing) != 0 {
		t.Fatalf("Missing = %+v, want empty", check.Missing)
	}
}

func TestStorageGate_AcknowledgeUnblocksWithoutTheDiskReappearing(t *testing.T) {
	g := NewStorageGate([]ExpectedDisk{expected("disk1", "wwn-1"), expected("disk2", "wwn-2")})
	g.Evaluate([]Identity{{WWN: "wwn-1"}}) // disk2 missing

	if g.Ready() {
		t.Fatal("Ready before acknowledgement, want false")
	}
	if err := g.Acknowledge(); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if !g.Ready() {
		t.Fatal("Ready after acknowledgement, want true")
	}
	if len(g.Missing()) != 1 {
		t.Fatalf("Missing after acknowledgement = %+v, want disk2 still reported", g.Missing())
	}
}

func TestStorageGate_AcknowledgeRefusesWhenNothingIsMissing(t *testing.T) {
	g := NewStorageGate([]ExpectedDisk{expected("disk1", "wwn-1")})
	g.Evaluate([]Identity{{WWN: "wwn-1"}})

	if err := g.Acknowledge(); err != ErrNothingToAcknowledge {
		t.Fatalf("Acknowledge: got %v, want ErrNothingToAcknowledge", err)
	}
}

// TestStorageGate_ReacquiringTheDiskClearsAStaleAcknowledgement is the
// case Q69's degraded-state design must not get wrong the other way: an
// acknowledgement that outlives the disk actually reappearing would let a
// later, genuinely different missing disk sail through Ready() on an old
// human decision it was never about.
func TestStorageGate_ReacquiringTheDiskClearsAStaleAcknowledgement(t *testing.T) {
	g := NewStorageGate([]ExpectedDisk{expected("disk1", "wwn-1"), expected("disk2", "wwn-2")})
	g.Evaluate([]Identity{{WWN: "wwn-1"}})
	if err := g.Acknowledge(); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	g.Evaluate([]Identity{{WWN: "wwn-1"}, {WWN: "wwn-2"}})
	if !g.Ready() {
		t.Fatal("Ready once disk2 reappears, want true")
	}

	// disk2 goes missing again; the earlier acknowledgement must not cover
	// this new occurrence.
	g.Evaluate([]Identity{{WWN: "wwn-1"}})
	if g.Ready() {
		t.Fatal("Ready with disk2 missing a second time and no fresh acknowledgement, want false")
	}
}
