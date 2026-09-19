package job

// storageClasses are doc 01 §4's "three storage classes" — Parity,
// Array-write and Topology. Parity and Topology each exclude every job in
// any of the three, with no scoping (a sync anywhere in the array
// conflicts with a disk_add anywhere in the array). Array-write only
// excludes another Array-write job when their resource scopes overlap
// ("same disks") — this is the one row in doc 01 §4's table that is
// scoped rather than global.
var storageClasses = map[Class]bool{
	ClassParity:     true,
	ClassArrayWrite: true,
	ClassTopology:   true,
}

// IsStorageClass reports whether c is one of doc 01 §4's three storage
// classes — Parity, Array-write or Topology. Self-update, rollback and
// reboot consult this so they never run over a live sync (Q67, Q68).
func IsStorageClass(c Class) bool {
	return storageClasses[c]
}

// conflicts reports whether a job of class/scope a and a job of
// class/scope b may never run at the same time, per doc 01 §4's
// mutually-exclusive-class table:
//
//   - Parity excludes Parity, Array-write and Topology — globally.
//   - Array-write excludes Parity and Topology globally, and excludes
//     another Array-write only on the same disks (scope overlap).
//   - Topology excludes Parity, Array-write and Topology — globally
//     ("everything in the three storage classes").
//   - Service excludes another Service job on the same container (scope
//     overlap).
//   - VM excludes another VM job on the same VM (scope overlap).
//   - Nothing else conflicts: Service and VM are independent of the
//     storage classes and of each other.
func conflicts(a, b Class, scopeA, scopeB []string) bool {
	if storageClasses[a] && storageClasses[b] {
		if a == ClassArrayWrite && b == ClassArrayWrite {
			return scopeOverlaps(scopeA, scopeB)
		}
		return true
	}
	if a == ClassService && b == ClassService {
		return scopeOverlaps(scopeA, scopeB)
	}
	if a == ClassVM && b == ClassVM {
		return scopeOverlaps(scopeA, scopeB)
	}
	return false
}

// scopeOverlaps reports whether two jobs' resource scopes (disk ids,
// container ids or VM ids, depending on class) share any entry. An empty
// scope on either side means "this job's exact resources aren't known" —
// treated as overlapping everything of the same scoped class, the
// conservative reading a safety-critical exclusion check must take: a
// scope this package can't confirm is disjoint is never treated as
// disjoint.
func scopeOverlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; ok {
			return true
		}
	}
	return false
}
