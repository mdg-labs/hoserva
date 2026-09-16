package job

import "testing"

// TestConflicts_ClassTable exercises every row of doc 01 §4's
// mutually-exclusive-class table, including the scoped "same disks" /
// "same container" / "same VM" cases: a running parity/array-write/service
// sync must corrupt nothing, so every one of these is safety-critical.
func TestConflicts_ClassTable(t *testing.T) {
	cases := []struct {
		name   string
		a, b   Class
		scopeA []string
		scopeB []string
		want   bool
	}{
		// Parity excludes Parity, Array-write, Topology (globally).
		{"parity vs parity", ClassParity, ClassParity, nil, nil, true},
		{"parity vs array_write", ClassParity, ClassArrayWrite, []string{"disk-1"}, []string{"disk-2"}, true},
		{"parity vs topology", ClassParity, ClassTopology, nil, nil, true},

		// Array-write excludes Parity, Topology (globally), and another
		// Array-write only on the same disks.
		{"array_write vs parity", ClassArrayWrite, ClassParity, []string{"disk-1"}, nil, true},
		{"array_write vs topology", ClassArrayWrite, ClassTopology, []string{"disk-1"}, nil, true},
		{"array_write vs array_write, same disk", ClassArrayWrite, ClassArrayWrite, []string{"disk-1"}, []string{"disk-1", "disk-2"}, true},
		{"array_write vs array_write, different disks", ClassArrayWrite, ClassArrayWrite, []string{"disk-1"}, []string{"disk-2"}, false},
		{"array_write vs array_write, unknown scope is conservative", ClassArrayWrite, ClassArrayWrite, nil, []string{"disk-2"}, true},

		// Topology excludes Parity, Array-write, Topology — everything in
		// the three storage classes.
		{"topology vs parity", ClassTopology, ClassParity, nil, nil, true},
		{"topology vs array_write", ClassTopology, ClassArrayWrite, nil, []string{"disk-1"}, true},
		{"topology vs topology", ClassTopology, ClassTopology, nil, nil, true},

		// Service excludes another Service job only on the same container.
		{"service vs service, same container", ClassService, ClassService, []string{"c1"}, []string{"c1"}, true},
		{"service vs service, different container", ClassService, ClassService, []string{"c1"}, []string{"c2"}, false},
		{"service vs service, unknown scope is conservative", ClassService, ClassService, nil, []string{"c2"}, true},

		// VM excludes another VM job only on the same VM.
		{"vm vs vm, same vm", ClassVM, ClassVM, []string{"vm1"}, []string{"vm1"}, true},
		{"vm vs vm, different vm", ClassVM, ClassVM, []string{"vm1"}, []string{"vm2"}, false},
		{"vm vs vm, unknown scope is conservative", ClassVM, ClassVM, nil, []string{"vm2"}, true},

		// Topology is not excluded by, and does not exclude, Service or VM
		// (doc 01 §4's Topology row only names the three storage classes).
		{"topology vs service", ClassTopology, ClassService, nil, []string{"c1"}, false},
		{"topology vs vm", ClassTopology, ClassVM, nil, []string{"vm1"}, false},

		// Service and VM are independent of each other and of the storage
		// classes generally.
		{"service vs vm", ClassService, ClassVM, []string{"c1"}, []string{"vm1"}, false},
		{"service vs parity", ClassService, ClassParity, []string{"c1"}, nil, false},
		{"service vs array_write", ClassService, ClassArrayWrite, []string{"c1"}, []string{"disk-1"}, false},
		{"vm vs parity", ClassVM, ClassParity, []string{"vm1"}, nil, false},
		{"vm vs array_write", ClassVM, ClassArrayWrite, []string{"vm1"}, []string{"disk-1"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conflicts(tc.a, tc.b, tc.scopeA, tc.scopeB); got != tc.want {
				t.Errorf("conflicts(%s, %s, %v, %v) = %v, want %v", tc.a, tc.b, tc.scopeA, tc.scopeB, got, tc.want)
			}
			// The relation is symmetric: doc 01 §4's table is written from
			// each class's own row, but "A excludes B" and "B excludes A"
			// describe the same real-world conflict.
			if got := conflicts(tc.b, tc.a, tc.scopeB, tc.scopeA); got != tc.want {
				t.Errorf("conflicts(%s, %s, %v, %v) [reversed] = %v, want %v", tc.b, tc.a, tc.scopeB, tc.scopeA, got, tc.want)
			}
		})
	}
}

func TestClassOfAndResumable(t *testing.T) {
	for typ := range classOf {
		if err := ValidateType(typ); err != nil {
			t.Errorf("ValidateType(%s): %v", typ, err)
		}
	}
	if err := ValidateType(Type("not_a_real_type")); err == nil {
		t.Error("ValidateType(bogus type) = nil, want error")
	}

	for _, typ := range []Type{TypeMover, TypeRebalance, TypeEvacuation, TypeShareRelocation} {
		if !Resumable(typ) {
			t.Errorf("Resumable(%s) = false, want true (Q29)", typ)
		}
	}
	for _, typ := range []Type{TypeSync, TypeScrub, TypeFix, TypeCheck, TypeDiskFormat, TypeVMStart} {
		if Resumable(typ) {
			t.Errorf("Resumable(%s) = true, want false (Q29: only mover, rebalance, evacuation, share_relocation resume)", typ)
		}
	}
}
