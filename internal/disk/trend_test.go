package disk

import "testing"

func TestComputeTrend(t *testing.T) {
	cases := []struct {
		prev, cur int
		want      Trend
	}{
		{0, 4, Rising},
		{4, 0, Falling},
		{4, 4, Stable},
		{0, 0, Stable},
	}
	for _, c := range cases {
		if got := ComputeTrend(c.prev, c.cur); got != c.want {
			t.Errorf("ComputeTrend(%d, %d) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func TestSMARTTrends_OverallErrorTrend_RisingWins(t *testing.T) {
	trends := SMARTTrends{Reallocated: Stable, Pending: Rising, Uncorrectable: Falling, CRC: Stable}
	if got := trends.OverallErrorTrend(); got != Rising {
		t.Fatalf("OverallErrorTrend: got %v, want Rising", got)
	}
}

func TestSMARTTrends_OverallErrorTrend_TemperatureIgnored(t *testing.T) {
	// A rising temperature alone must not surface as the SMART attribute
	// trend — doc 02 §4 alerts on temperature by absolute threshold, not
	// trend, so it must not masquerade as an error-count regression.
	trends := SMARTTrends{Temperature: Rising}
	if got := trends.OverallErrorTrend(); got != Stable {
		t.Fatalf("OverallErrorTrend: got %v, want Stable (temperature must not count)", got)
	}
}

func TestSMARTTrends_OverallErrorTrend_Falling(t *testing.T) {
	trends := SMARTTrends{Reallocated: Falling}
	if got := trends.OverallErrorTrend(); got != Falling {
		t.Fatalf("OverallErrorTrend: got %v, want Falling", got)
	}
}

func TestTrendTracker_FirstReadingIsStable(t *testing.T) {
	tr := NewTrendTracker()
	got := tr.Update("/dev/sdb", SMARTReport{ReallocatedSectors: 4})
	if got != (SMARTTrends{}) {
		t.Fatalf("Update (first reading): got %+v, want the zero value", got)
	}
}

func TestTrendTracker_DetectsReallocatedSectorRegression(t *testing.T) {
	tr := NewTrendTracker()
	tr.Update("/dev/sdb", SMARTReport{ReallocatedSectors: 0})
	got := tr.Update("/dev/sdb", SMARTReport{ReallocatedSectors: 4})
	if got.Reallocated != Rising {
		t.Fatalf("Update: ReallocatedSectors 0 -> 4 trend = %v, want Rising", got.Reallocated)
	}
}

func TestTrendTracker_TracksDevicesIndependently(t *testing.T) {
	tr := NewTrendTracker()
	tr.Update("/dev/sdb", SMARTReport{ReallocatedSectors: 4})
	got := tr.Update("/dev/sdc", SMARTReport{ReallocatedSectors: 9})
	if got != (SMARTTrends{}) {
		t.Fatalf("Update: a second device's first reading was compared against another device's history: %+v", got)
	}
}
