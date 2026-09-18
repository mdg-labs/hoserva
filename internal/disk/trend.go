package disk

import "sync"

// ComputeTrend classifies cur against prev — the direction doc 02 §4 says
// is the signal worth alerting on, not the absolute value ("reallocated
// sectors going from 0 to 4 is the signal; the absolute number is not").
func ComputeTrend(prev, cur int) Trend {
	switch {
	case cur > prev:
		return Rising
	case cur < prev:
		return Falling
	default:
		return Stable
	}
}

// SMARTTrends is the per-attribute trend doc 02 §4 asks tracked:
// reallocated, pending and offline-uncorrectable sectors, the interface
// CRC error count, and temperature — each compared against the same
// disk's previous poll.
type SMARTTrends struct {
	Reallocated   Trend
	Pending       Trend
	Uncorrectable Trend
	CRC           Trend
	Temperature   Trend
}

// ComputeSMARTTrends compares two successive SMART readings for the same
// disk, attribute by attribute.
func ComputeSMARTTrends(prev, cur SMARTReport) SMARTTrends {
	return SMARTTrends{
		Reallocated:   ComputeTrend(prev.ReallocatedSectors, cur.ReallocatedSectors),
		Pending:       ComputeTrend(prev.PendingSectors, cur.PendingSectors),
		Uncorrectable: ComputeTrend(prev.OfflineUncorrectable, cur.OfflineUncorrectable),
		CRC:           ComputeTrend(prev.CRCErrors, cur.CRCErrors),
		Temperature:   ComputeTrend(prev.TemperatureCelsius, cur.TemperatureCelsius),
	}
}

// OverallErrorTrend collapses SMARTTrends into the single Trend
// SMARTReport carries: any of the four error-count attributes rising is
// the alert-worthy case doc 02 §4 names, and it outweighs everything
// else. Temperature is excluded here — doc 02 §4 alerts on temperature by
// an absolute threshold, not its trend, so it stays in SMARTTrends for
// its own history graph (doc 03 §3.4) without affecting this summary.
func (t SMARTTrends) OverallErrorTrend() Trend {
	switch {
	case t.Reallocated == Rising || t.Pending == Rising || t.Uncorrectable == Rising || t.CRC == Rising:
		return Rising
	case t.Reallocated == Falling || t.Pending == Falling || t.Uncorrectable == Falling || t.CRC == Falling:
		return Falling
	default:
		return Stable
	}
}

// TrendTracker keeps each disk's last SMART reading so a fresh poll can
// be classified against it.
type TrendTracker struct {
	mu   sync.Mutex
	last map[string]SMARTReport
}

// NewTrendTracker returns an empty TrendTracker.
func NewTrendTracker() *TrendTracker {
	return &TrendTracker{last: make(map[string]SMARTReport)}
}

// Update records cur as dev's latest reading and returns the per-attribute
// trend against whatever reading preceded it. A disk's first-ever
// reading has nothing to compare against, so every attribute reports
// Stable. The caller is responsible for never passing a Skipped report —
// a standby poll's all-zero attributes would otherwise be read back as a
// real improvement on the next poll.
func (t *TrendTracker) Update(dev string, cur SMARTReport) SMARTTrends {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev, known := t.last[dev]
	t.last[dev] = cur
	if !known {
		return SMARTTrends{}
	}
	return ComputeSMARTTrends(prev, cur)
}
