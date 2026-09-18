package pool

// CreatePolicy is one of mergerfs's create policies (doc 02 §1, Q11),
// named by the exact string mergerfs's own category.create option
// takes. S6 and S8 (doc 08) confirmed all four are accepted and behave
// as documented on Debian 13's mergerfs 2.40.2.
type CreatePolicy string

const (
	// KeepFoldersTogether (mspmfs) prefers a branch that already holds
	// the path, falling back to the parent directory — and its
	// parent — rather than failing ENOSPC (Q11, confirmed by S6). The
	// default for new shares.
	KeepFoldersTogether CreatePolicy = "mspmfs"
	// BalanceAcrossDisks (mfs) sends a new file to the branch with the
	// most free space.
	BalanceAcrossDisks CreatePolicy = "mfs"
	// QuietDisks (lfs) sends a new file to the least-free branch that
	// still fits, filling one disk at a time to maximise spindown.
	QuietDisks CreatePolicy = "lfs"
	// FillDisksInOrder (ff) sends a new file to the first branch with
	// room, in branch order — used for Unraid shares imported with
	// Fill-up allocation.
	FillDisksInOrder CreatePolicy = "ff"
)

// DefaultCreatePolicy is Q11's default for a new share, and the policy
// the catch-all pool always uses (doc 02 §1: "catch-all, default
// policy").
const DefaultCreatePolicy = KeepFoldersTogether

// Label returns p's plain-language UI label (doc 02 §1, Q11) —
// mergerfs's own policy names are opaque and are never shown to a
// user directly.
func (p CreatePolicy) Label() string {
	switch p {
	case KeepFoldersTogether:
		return "Keep folders together"
	case BalanceAcrossDisks:
		return "Balance across disks"
	case QuietDisks:
		return "Quiet disks"
	case FillDisksInOrder:
		return "Fill disks in order"
	default:
		return string(p)
	}
}
