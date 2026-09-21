package cache

import "testing"

func TestThresholdExceeded(t *testing.T) {
	cases := []struct {
		name        string
		used, total int64
		pct         float64
		want        bool
	}{
		{"below default", 60, 100, DefaultThresholdPercent, false},
		{"exactly at default", 70, 100, DefaultThresholdPercent, true},
		{"above default", 90, 100, DefaultThresholdPercent, true},
		{"unknown total never exceeds", 90, 0, DefaultThresholdPercent, false},
		{"negative total never exceeds", 90, -1, DefaultThresholdPercent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThresholdExceeded(tc.used, tc.total, tc.pct); got != tc.want {
				t.Errorf("ThresholdExceeded(%d, %d, %v) = %v, want %v", tc.used, tc.total, tc.pct, got, tc.want)
			}
		})
	}
}
