package backup

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// priorRetentionKeepers is retentionKeepers as it was before #478: every
// archive, pre-change ones included, fed the daily/weekly/monthly tiers.
// The tests below hold the current rule against it, so a pre-change archive
// the old rule kept is never deleted without an ordinary archive taking its
// place.
func priorRetentionKeepers(entries []archiveEntry, ret Retention, justWritten string) map[string]bool {
	keep := map[string]bool{justWritten: true}
	preChangeKept := 0
	for _, e := range entries {
		if e.reason == ReasonNone {
			continue
		}
		if preChangeKept >= preChangeKeepCount {
			break
		}
		keep[e.name] = true
		preChangeKept++
	}

	byDay := map[string]archiveEntry{}
	byWeek := map[string]archiveEntry{}
	byMonth := map[string]archiveEntry{}
	for _, e := range entries {
		day := e.modTime.Format("2006-01-02")
		if cur, ok := byDay[day]; !ok || e.modTime.After(cur.modTime) {
			byDay[day] = e
		}
		year, week := e.modTime.ISOWeek()
		weekKey := fmt.Sprintf("%04d-W%02d", year, week)
		if cur, ok := byWeek[weekKey]; !ok || e.modTime.After(cur.modTime) {
			byWeek[weekKey] = e
		}
		monthKey := e.modTime.Format("2006-01")
		if cur, ok := byMonth[monthKey]; !ok || e.modTime.After(cur.modTime) {
			byMonth[monthKey] = e
		}
	}
	for i, k := range sortedKeys(byDay) {
		if i < ret.Daily {
			keep[byDay[k].name] = true
		}
	}
	for i, k := range sortedKeys(byWeek) {
		if i < ret.Weekly {
			keep[byWeek[k].name] = true
		}
	}
	for i, k := range sortedKeys(byMonth) {
		if i < ret.Monthly {
			keep[byMonth[k].name] = true
		}
	}
	return keep
}

func writeArchiveAt(t *testing.T, dir string, mod time.Time, reason Reason) string {
	t.Helper()
	name := archiveName(testInstallation, mod, reason, 0)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return name
}

func assertSurvivors(t *testing.T, dir string, names []string, want []bool) {
	t.Helper()
	for i, name := range names {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want[i] {
			t.Errorf("archive %q kept = %v, want %v", name, err == nil, want[i])
		}
	}
}

var defaultRetention = Retention{Daily: 7, Weekly: 4, Monthly: 6}

// TestRetentionPrune_OverflowPreChangeKeptWithoutOrdinaryArchives covers a
// destination holding only pre-change archives (a tripped threshold guard or
// a disabled config-backup step stops the nightly ones). Beyond the newest
// five, a pre-change archive that is the newest of a day the daily tier
// keeps survives, as it did before #478. It fails with the sixth and
// seventh archives pruned when retentionKeepers keeps no overflow archive.
func TestRetentionPrune_OverflowPreChangeKeptWithoutOrdinaryArchives(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	var names []string
	for i := 0; i < 8; i++ {
		names = append(names, writeArchiveAt(t, dir, now.AddDate(0, 0, -7*i), ReasonPreTopology))
	}

	dest := Destination{Path: dir, Retention: defaultRetention}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, names[0]); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	assertSurvivors(t, dir, names, []bool{true, true, true, true, true, true, true, false})
}

// TestRetentionPrune_OverflowPreChangeKeptForMonthWithoutOrdinaryArchive is
// the upgrade case: a destination whose June nightly archive an earlier
// prune already deleted in favour of that day's pre-update archive. With
// five newer pre-change archives, the pre-update archive is beyond the bound
// but is still June's only backup, so it survives. It fails with that
// archive pruned when retentionKeepers keeps no overflow archive.
func TestRetentionPrune_OverflowPreChangeKeptForMonthWithoutOrdinaryArchive(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	preUpdateJune := writeArchiveAt(t, dir, time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC), ReasonPreUpdate)
	names := []string{preUpdateJune}
	for d := 5; d <= 9; d++ {
		names = append(names, writeArchiveAt(t, dir, time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC), ReasonPreImport))
	}
	nightly := writeArchiveAt(t, dir, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), ReasonNone)
	names = append(names, nightly)

	dest := Destination{Path: dir, Retention: defaultRetention}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, nightly); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	assertSurvivors(t, dir, names, []bool{true, true, true, true, true, true, true})
}

// TestRetentionPrune_OverflowPreChangeYieldsToOrdinaryArchive: where a
// period does hold an ordinary archive, an overflow pre-change archive in it
// is pruned and the ordinary one kept.
func TestRetentionPrune_OverflowPreChangeYieldsToOrdinaryArchive(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	nightlyJune := writeArchiveAt(t, dir, time.Date(2026, 6, 30, 3, 0, 0, 0, time.UTC), ReasonNone)
	preUpdateJune := writeArchiveAt(t, dir, time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC), ReasonPreUpdate)
	names := []string{nightlyJune, preUpdateJune}
	for d := 5; d <= 9; d++ {
		names = append(names, writeArchiveAt(t, dir, time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC), ReasonPreImport))
	}
	nightly := writeArchiveAt(t, dir, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), ReasonNone)
	names = append(names, nightly)

	dest := Destination{Path: dir, Retention: defaultRetention}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, nightly); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	assertSurvivors(t, dir, names, []bool{true, false, true, true, true, true, true, true})
}

// TestRetentionPrune_PreChangeSlotsCoveredByOrdinaryArchive: the prior rule
// kept the 06-30 and 07-01 pre-update archives as ISO week 27's and June's
// only slots. The ordinary 06-29 archive holds both periods, so it is kept
// and the two pre-update archives are pruned. Every slot the prior rule
// filled stays filled, with fewer archives than before.
func TestRetentionPrune_PreChangeSlotsCoveredByOrdinaryArchive(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	ordinaryJune := writeArchiveAt(t, dir, time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC), ReasonNone)
	preUpdateJune := writeArchiveAt(t, dir, time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC), ReasonPreUpdate)
	preUpdateJuly := writeArchiveAt(t, dir, time.Date(2026, 7, 1, 21, 0, 0, 0, time.UTC), ReasonPreUpdate)
	ordinaryJuly := writeArchiveAt(t, dir, time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC), ReasonNone)
	names := []string{ordinaryJune, preUpdateJune, preUpdateJuly, ordinaryJuly}
	for h := 10; h <= 14; h++ {
		names = append(names, writeArchiveAt(t, dir, time.Date(2026, 8, 3, h, 0, 0, 0, time.UTC), ReasonPreImport))
	}

	dest := Destination{Path: dir, Retention: Retention{Daily: 1, Weekly: 4, Monthly: 6}}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, names[len(names)-1]); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	assertSurvivors(t, dir, names, []bool{true, false, false, true, true, true, true, true, true})
}

// TestRetentionKeepers_AgainstPriorRule runs seeded random destinations
// through the current rule and the pre-#478 one. The current rule must keep
// every ordinary archive the ordinary tiers alone would keep, so a
// pre-change archive never evicts one. It must also keep every tier slot the
// prior rule filled: for each of the daily, weekly and monthly tiers and each
// period the prior rule kept an archive for, the newest ordinary archive of
// that period is kept when it holds one, and the pre-change archive the prior
// rule kept otherwise. It need not keep as many archives: a pre-change
// archive is pruned where an ordinary archive now covers its slot.
func TestRetentionKeepers_AgainstPriorRule(t *testing.T) {
	rng := rand.New(rand.NewSource(478))
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	reasons := []Reason{ReasonNone, ReasonNone, ReasonPreImport, ReasonPreUpdate, ReasonPreTopology}
	tiers := []struct {
		name   string
		period func(time.Time) string
		count  func(Retention) int
	}{
		{"daily", func(t time.Time) string { return t.Format("2006-01-02") }, func(r Retention) int { return r.Daily }},
		{"weekly", func(t time.Time) string {
			year, week := t.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", year, week)
		}, func(r Retention) int { return r.Weekly }},
		{"monthly", func(t time.Time) string { return t.Format("2006-01") }, func(r Retention) int { return r.Monthly }},
	}

	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(30)
		span := 1 + rng.Intn(400*24)
		entries := make([]archiveEntry, n)
		for i := range entries {
			mod := base.Add(-time.Duration(rng.Intn(span)) * time.Hour).Add(-time.Duration(rng.Intn(60)) * time.Minute)
			entries[i] = archiveEntry{
				name:    fmt.Sprintf("a%03d-%d", i, mod.Unix()),
				modTime: mod,
				reason:  reasons[rng.Intn(len(reasons))],
			}
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].modTime.After(entries[j].modTime) })
		ret := Retention{Daily: rng.Intn(9), Weekly: rng.Intn(6), Monthly: rng.Intn(8)}
		justWritten := entries[0].name

		got := retentionKeepers(entries, ret, base, justWritten)
		prior := priorRetentionKeepers(entries, ret, justWritten)

		var ordinary []archiveEntry
		for _, e := range entries {
			if e.reason == ReasonNone {
				ordinary = append(ordinary, e)
			}
		}
		ordinaryOnly := priorRetentionKeepers(ordinary, ret, justWritten)
		for _, e := range ordinary {
			if ordinaryOnly[e.name] && !got[e.name] {
				t.Fatalf("iter %d: ordinary archive %s dropped by a pre-change archive; ret=%+v", iter, e.name, ret)
			}
		}

		for _, tier := range tiers {
			newest := newestPerPeriod(entries, tier.period)
			newestOrdinary := newestPerPeriod(ordinary, tier.period)
			for i, period := range sortedKeys(newest) {
				if i >= tier.count(ret) {
					break
				}
				priorSlot := newest[period]
				if !prior[priorSlot.name] {
					t.Fatalf("iter %d: %s period %s: prior rule dropped %s; ret=%+v", iter, tier.name, period, priorSlot.name, ret)
				}
				want := priorSlot
				if o, ok := newestOrdinary[period]; ok {
					want = o
				}
				if !got[want.name] {
					t.Fatalf("iter %d: %s period %s: %s (%s) is not kept although the prior rule filled the slot; ret=%+v",
						iter, tier.name, period, want.name, want.reason, ret)
				}
			}
		}
	}
}
