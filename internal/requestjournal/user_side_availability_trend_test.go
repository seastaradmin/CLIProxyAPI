package requestjournal

import (
	"testing"
	"time"
)

func TestBucketSizeForWindow(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{15 * time.Minute, time.Minute},
		{time.Hour, 5 * time.Minute},
		{6 * time.Hour, 15 * time.Minute},
		{24 * time.Hour, time.Hour},
		{7 * 24 * time.Hour, 6 * time.Hour},
		{30 * 24 * time.Hour, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := bucketSizeForWindow(c.window); got != c.want {
			t.Fatalf("bucketSizeForWindow(%v) = %v, want %v", c.window, got, c.want)
		}
	}
}

func TestBuildAvailabilityTrendBucketsRequestsByTerminalAttempt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	window := 24 * time.Hour
	start := now.Add(-window)

	// One request resolved at the very start, one hard-failed an hour in.
	records := []Record{
		chRec("r1-ok", "req1", start.Unix()+10, false, "p", "a", "m", "http", "", ""),
		chRec("r2-bad", "req2", start.Add(time.Hour).Unix()+10, true, "p", "a", "m", "http", "quota", ""),
	}
	buckets := BuildAvailabilityTrend(records, window, now, UserSideFilters{})
	if len(buckets) != 24 { // 24h window / 1h = 24 buckets
		t.Fatalf("got %d buckets, want 24", len(buckets))
	}
	// First bucket: the resolved request.
	if buckets[0].Resolved != 1 || buckets[0].HardFailed != 0 || buckets[0].Availability != 1.0 {
		t.Fatalf("bucket[0] = %+v, want 1 resolved / 0 hard-failed / 1.0", buckets[0])
	}
	// Second bucket: the hard-failed request.
	if buckets[1].Resolved != 0 || buckets[1].HardFailed != 1 || buckets[1].Availability != 0.0 {
		t.Fatalf("bucket[1] = %+v, want 0 resolved / 1 hard-failed / 0.0", buckets[1])
	}
	// Empty middle bucket.
	if buckets[2].Resolved != 0 || buckets[2].HardFailed != 0 {
		t.Fatalf("bucket[2] should be empty, got %+v", buckets[2])
	}
}

func TestBuildAvailabilityTrendResolvedByLatestAttempt(t *testing.T) {
	// A request with an early failure and a later success is resolved, bucketed
	// at its terminal attempt's time.
	now := time.Now().UTC().Truncate(time.Hour)
	window := time.Hour
	start := now.Add(-window)
	records := []Record{
		chRec("r-early", "req", start.Add(10*time.Minute).Unix(), true, "p", "a", "m", "http", "quota", ""),
		chRec("r-late", "req", start.Add(20*time.Minute).Unix(), false, "p", "a", "m", "http", "", ""),
	}
	buckets := BuildAvailabilityTrend(records, window, now, UserSideFilters{})
	// 1h window / 5m = 12 buckets. The request lands in the 20-minute bucket.
	if len(buckets) != 12 {
		t.Fatalf("got %d buckets, want 12", len(buckets))
	}
	idx := 20 / 5 // 4th bucket (20min mark)
	if buckets[idx].Resolved != 1 || buckets[idx].HardFailed != 0 {
		t.Fatalf("bucket[%d] = %+v, want 1 resolved (terminal success)", idx, buckets[idx])
	}
}

func TestBuildAvailabilityTrendFilters(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	window := time.Hour
	start := now.Add(-window)
	records := []Record{
		chRec("a", "req-a", start.Unix()+10, true, "p1", "a", "m1", "http", "quota", ""),
		chRec("b", "req-b", start.Unix()+20, true, "p2", "a", "m2", "http", "quota", ""),
	}
	// provider filter p1 → only req-a counted.
	buckets := BuildAvailabilityTrend(records, window, now, UserSideFilters{Provider: "p1"})
	total := int64(0)
	for _, b := range buckets {
		total += b.HardFailed
	}
	if total != 1 {
		t.Fatalf("provider-filtered total hard-failed = %d, want 1", total)
	}
	// model filter m2 → only req-b counted.
	buckets = BuildAvailabilityTrend(records, window, now, UserSideFilters{Model: "m2"})
	total = 0
	for _, b := range buckets {
		total += b.HardFailed
	}
	if total != 1 {
		t.Fatalf("model-filtered total hard-failed = %d, want 1", total)
	}
}

func TestBuildAvailabilityTrendContiguousAlignedBuckets(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	window := 15 * time.Minute
	buckets := BuildAvailabilityTrend(nil, window, now, UserSideFilters{})
	if len(buckets) != 15 { // 15m / 1m
		t.Fatalf("got %d buckets, want 15", len(buckets))
	}
	// Buckets are contiguous and aligned to the window start.
	for i := 1; i < len(buckets); i++ {
		if !buckets[i].Time.Equal(buckets[i-1].Time.Add(time.Minute)) {
			t.Fatalf("bucket %d not contiguous: %v -> %v", i, buckets[i-1].Time, buckets[i].Time)
		}
	}
}
