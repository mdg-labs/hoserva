package metrics

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestStore_InsertAndValues(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	if err := s.Insert(ctx, Sample{Metric: "smart_temperature", Subject: "/dev/sdb", At: at, Value: 34}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	values, err := s.Values(ctx, Raw, "smart_temperature", "/dev/sdb")
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("len(values) = %d, want 1", len(values))
	}
	if !values[0].At.Equal(at) || !closeEnough(values[0].Value, 34) {
		t.Fatalf("values[0] = %+v, want At=%v Value=34", values[0], at)
	}
}

func TestStore_Insert_SameSecondOverwrites(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	if err := s.Insert(ctx, Sample{Metric: "cpu_percent", At: at, Value: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, Sample{Metric: "cpu_percent", At: at, Value: 20}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Count(ctx, Raw)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Count(Raw) = %d, want 1 (second Insert should overwrite, not add a row)", n)
	}
	values, err := s.Values(ctx, Raw, "cpu_percent", "")
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(values[0].Value, 20) {
		t.Fatalf("values[0].Value = %v, want 20 (last write wins)", values[0].Value)
	}
}

// TestStore_Downsample_KeepsRecentRawUntouched confirms Q74's "raw samples
// for 48 hours": a sample well inside that window survives Downsample as
// a raw row, not rolled into an hourly average.
func TestStore_Downsample_KeepsRecentRawUntouched(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	if err := s.Insert(ctx, Sample{Metric: "cpu_percent", At: now.Add(-1 * time.Hour), Value: 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Downsample(ctx, now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}

	if n, err := s.Count(ctx, Raw); err != nil || n != 1 {
		t.Fatalf("Count(Raw) = %d, %v, want 1, nil", n, err)
	}
	if n, err := s.Count(ctx, Hourly); err != nil || n != 0 {
		t.Fatalf("Count(Hourly) = %d, %v, want 0, nil", n, err)
	}
}

// TestStore_Downsample_RollsExpiredRawIntoHourlyAverage confirms the
// rollup itself: several raw samples in one closed hour bucket become a
// single hourly row holding their average, and the raw rows are gone.
func TestStore_Downsample_RollsExpiredRawIntoHourlyAverage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	// now - RawRetention is Sept 16 12:00; this bucket's end (Sept 16
	// 08:00) is well before that, so it's fully aged out.
	bucketStart := now.Add(-52 * time.Hour).Truncate(time.Hour)
	samples := []float64{10, 20, 30}
	for i, v := range samples {
		at := bucketStart.Add(time.Duration(i*10) * time.Minute)
		if err := s.Insert(ctx, Sample{Metric: "smart_temperature", Subject: "/dev/sdb", At: at, Value: v}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Downsample(ctx, now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}

	if n, err := s.Count(ctx, Raw); err != nil || n != 0 {
		t.Fatalf("Count(Raw) after rollup = %d, %v, want 0, nil", n, err)
	}
	hourly, err := s.Values(ctx, Hourly, "smart_temperature", "/dev/sdb")
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 1 {
		t.Fatalf("len(hourly) = %d, want 1", len(hourly))
	}
	want := (10.0 + 20.0 + 30.0) / 3.0
	if !closeEnough(hourly[0].Value, want) {
		t.Fatalf("hourly average = %v, want %v", hourly[0].Value, want)
	}
	if !hourly[0].At.Equal(bucketStart.Truncate(time.Hour)) {
		t.Fatalf("hourly bucket = %v, want %v", hourly[0].At, bucketStart.Truncate(time.Hour))
	}
}

// TestStore_Downsample_RollsExpiredHourlyIntoDailyAverage confirms the
// second tier: hourly rows older than Q74's 90-day window roll into one
// daily average, and the hourly rows they came from are gone.
func TestStore_Downsample_RollsExpiredHourlyIntoDailyAverage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)

	dayStart := now.Add(-100 * 24 * time.Hour) // well past HourlyRetention (90d)
	for h, v := range []float64{4, 8} {
		at := dayStart.Add(time.Duration(h) * time.Hour)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO samples (resolution, metric, subject, at, value) VALUES (?, ?, ?, ?, ?)`,
			Hourly, "cpu_percent", "", at.Unix(), v); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Downsample(ctx, now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}

	if n, err := s.Count(ctx, Hourly); err != nil || n != 0 {
		t.Fatalf("Count(Hourly) after rollup = %d, %v, want 0, nil", n, err)
	}
	daily, err := s.Values(ctx, Daily, "cpu_percent", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 {
		t.Fatalf("len(daily) = %d, want 1", len(daily))
	}
	if !closeEnough(daily[0].Value, 6) {
		t.Fatalf("daily average = %v, want 6", daily[0].Value)
	}
}

// TestStore_Downsample_PrunesDailyPastTwoYears confirms Q74's outer
// bound: a daily sample older than two years is deleted outright, with
// nowhere further to roll up to.
func TestStore_Downsample_PrunesDailyPastTwoYears(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)

	tooOld := now.Add(-DailyRetention).Add(-24 * time.Hour)
	stillKept := now.Add(-DailyRetention).Add(24 * time.Hour)
	for _, at := range []time.Time{tooOld, stillKept} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO samples (resolution, metric, subject, at, value) VALUES (?, ?, ?, ?, ?)`,
			Daily, "cpu_percent", "", at.Unix(), 1.0); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Downsample(ctx, now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}

	daily, err := s.Values(ctx, Daily, "cpu_percent", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 {
		t.Fatalf("len(daily) = %d, want 1 (the too-old row should have been pruned)", len(daily))
	}
	if !daily[0].At.Equal(stillKept.Truncate(24 * time.Hour)) {
		t.Fatalf("surviving daily row = %v, want %v", daily[0].At, stillKept.Truncate(24*time.Hour))
	}
}

// TestStore_Downsample_IsIdempotent confirms a second, immediate call
// changes nothing — Downsample is meant to run repeatedly as a
// maintenance step (doc 01 §4), not exactly once.
func TestStore_Downsample_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	bucketStart := now.Add(-52 * time.Hour).Truncate(time.Hour)
	if err := s.Insert(ctx, Sample{Metric: "smart_temperature", Subject: "/dev/sdb", At: bucketStart, Value: 40}); err != nil {
		t.Fatal(err)
	}
	if err := s.Downsample(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Downsample(ctx, now); err != nil {
		t.Fatalf("second Downsample: %v", err)
	}

	hourly, err := s.Values(ctx, Hourly, "smart_temperature", "/dev/sdb")
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 1 {
		t.Fatalf("len(hourly) = %d, want 1 after two Downsample calls", len(hourly))
	}
}
