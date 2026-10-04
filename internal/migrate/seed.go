package migrate

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
)

// maxFloorKiB keeps a share's floor, in KiB, well inside what mergerfs parses
// as a size in bytes.
const maxFloorKiB = 1 << 40

// SeedSkip is a share or user the import does not create, with the reason.
type SeedSkip struct {
	Name   string
	Reason string
}

// SeedPlan is what the import creates from the scan's parsed configuration
// (doc 05 §4 steps 3, 4 and 15): every share the scan kept, with the settings
// Unraid's map to, and every account by name. It holds no password material:
// the scan never reads any.
type SeedPlan struct {
	Shares []share.SeedShare
	// Users are the account names, lower case, in order. They are created
	// share-only and without a working password.
	Users         []string
	SkippedShares []SeedSkip
	SkippedUsers  []SeedSkip
}

// SeedPlan maps the shares and users the scan parsed to what the import
// creates. A share whose name pool.ValidateShareName refuses is skipped with the
// reason and never renamed: a rename would break every container path and SMB
// client mapping that uses the old name. Every other share is planned.
func (imp Import) SeedPlan() SeedPlan {
	var plan SeedPlan
	known := map[string]bool{}
	for _, name := range imp.Users {
		user := strings.ToLower(name)
		switch {
		case share.ValidateAccountName(user) != nil:
			plan.SkippedUsers = append(plan.SkippedUsers, SeedSkip{Name: name, Reason: "the account name is not one Hoserva creates (lower-case letters, digits, '_', '.' and '-', starting with a letter or '_', at most 64 characters), and it is not renamed"})
		case known[user]:
		default:
			known[user] = true
			plan.Users = append(plan.Users, user)
		}
	}
	sort.Strings(plan.Users)
	for _, sh := range imp.Shares {
		if err := pool.ValidateShareName(sh.Name); err != nil {
			plan.SkippedShares = append(plan.SkippedShares, SeedSkip{Name: sh.Name, Reason: invalidShareNameReason})
			continue
		}
		plan.Shares = append(plan.Shares, seedShare(sh, known))
	}
	return plan
}

const invalidShareNameReason = "the name is not valid for Hoserva (letters, digits, '-' and '_', starting with a letter or digit); it is not renamed, because that would break every container path and SMB client mapping that uses it"

// seedShare is one share's mapping. users are the accounts the import creates.
func seedShare(sh Share, users map[string]bool) share.SeedShare {
	out := share.SeedShare{Name: sh.Name, SMB: share.SMB{Browseable: true}}
	var notes []string

	policy, exact, known := allocationPolicy(sh.Allocator)
	switch {
	case !known:
		policy = pool.DefaultCreatePolicy
		notes = append(notes, fmt.Sprintf("Unraid's allocation method %s is not one Hoserva maps, so the share uses the default policy, %s (%s).", allocationLabel(sh.Allocator), policy.Label(), policy))
	case !exact:
		notes = append(notes, fmt.Sprintf("Unraid's %s allocation has no equivalent in Hoserva: the share is mapped to %s (%s) (Q11).", allocationLabel(sh.Allocator), policy.Label(), policy))
	}
	out.CreatePolicy = policy

	if mode, ok := cacheModeFor(sh.UseCache); !ok {
		notes = append(notes, fmt.Sprintf("Unraid's cache setting %q is not one Hoserva maps, so the share is array-only.", sh.UseCache))
	} else if mode != pool.ArrayOnly {
		out.TargetCacheMode = mode
		notes = append(notes, fmt.Sprintf("Unraid's cache setting %s maps to the cache mode %s, applied once the cache disk exists; until then the share is array-only.", sh.UseCache, mode))
		if sh.UseCache == "prefer" {
			notes = append(notes, "Unraid's prefer also lets writes fall through to the array when the cache is full, which cache-only does not.")
		}
		if sh.CachePool != "" && sh.CachePool != "cache" {
			notes = append(notes, fmt.Sprintf("The share used the Unraid pool %q; Hoserva has one cache disk.", sh.CachePool))
		}
	}

	switch sh.Export {
	case "", "-":
	case "e":
		out.SMB.Enabled = true
	case "eh":
		out.SMB.Enabled = true
		out.SMB.Browseable = false
	case "et", "eth":
		out.SMB.Enabled = true
		out.SMB.Browseable = sh.Export == "et"
		notes = append(notes, "Unraid exported the share for Time Machine; Hoserva's Time Machine setting needs a maximum size, which the import does not carry over, so set it on the share to turn it on.")
	default:
		out.SMB.Enabled = true
		out.SMB.Browseable = false
		notes = append(notes, fmt.Sprintf("Unraid's export setting %q is not one Hoserva maps, so the share is exported hidden (not browseable).", sh.Export))
	}
	var listed []string
	switch sh.Security {
	case "public":
		out.SMB.Guest = true
	case "secure":
		notes = append(notes, "Unraid's secure security lets everyone read without logging in and only the write list write; Hoserva has no such mode, so guest access is off and the read and write lists become per-user access.")
		listed = append(listed, sh.ReadList...)
		listed = append(listed, sh.WriteList...)
	case "private":
		listed = append(listed, sh.ReadList...)
		listed = append(listed, sh.WriteList...)
	default:
		notes = append(notes, fmt.Sprintf("Unraid's security setting %q is not one Hoserva maps, so guest access is off.", sh.Security))
	}
	if len(listed) > 0 {
		access := map[string]string{}
		var unknown []string
		for _, name := range sh.ReadList {
			if u := strings.ToLower(name); users[u] {
				access[u] = "read-only"
			}
		}
		for _, name := range sh.WriteList {
			if u := strings.ToLower(name); users[u] {
				access[u] = "read-write"
			}
		}
		for _, name := range listed {
			if u := strings.ToLower(name); !users[u] {
				unknown = append(unknown, name)
			}
		}
		for u, a := range access {
			out.Access = append(out.Access, share.SeedAccess{Username: u, Access: a})
		}
		sort.Slice(out.Access, func(i, j int) bool { return out.Access[i].Username < out.Access[j].Username })
		if len(unknown) > 0 {
			sort.Strings(unknown)
			notes = append(notes, fmt.Sprintf("The read and write lists name accounts that are not among the imported ones, so they get no access: %s.", joinNames(unknown)))
		}
	}

	if sh.Floor != "" && sh.Floor != "0" {
		if kib, err := strconv.ParseUint(sh.Floor, 10, 64); err != nil || kib > maxFloorKiB {
			notes = append(notes, fmt.Sprintf("Unraid's floor %q is not a size in KiB Hoserva reads, so the array's minimum free space applies.", sh.Floor))
		} else {
			out.MinFreeSpace = strconv.FormatUint(kib, 10) + "K"
		}
	}
	if sh.SplitLevel != "" {
		notes = append(notes, fmt.Sprintf("Unraid's split level %s has no equivalent: Hoserva has no split-level setting, and the share's create policy (%s) decides placement (Q11).", sh.SplitLevel, policy.Label()))
	}
	if len(sh.Include) > 0 {
		notes = append(notes, "Unraid limited the share to the disks "+joinNames(sh.Include)+"; Hoserva has no per-share disk selection.")
	}
	if len(sh.Exclude) > 0 {
		notes = append(notes, "Unraid kept the share off the disks "+joinNames(sh.Exclude)+"; Hoserva has no per-share disk selection.")
	}
	out.Notes = notes
	return out
}

// SeedPlan is the plan for the session's report. It fails when the session
// holds none.
func (s *Service) SeedPlan(ctx context.Context) (SeedPlan, error) {
	s.mu.Lock()
	sess, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return SeedPlan{}, err
	}
	if sess.Report == nil {
		return SeedPlan{}, ErrImportNoReport
	}
	return sess.Report.Import.SeedPlan(), nil
}
