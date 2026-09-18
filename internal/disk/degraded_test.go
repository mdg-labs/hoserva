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
