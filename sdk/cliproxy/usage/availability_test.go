package usage

import (
	"context"
	"testing"
	"time"
)

func TestAvailabilityAggregatorSnapshot(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	key := AvailabilityKey{Provider: "claude", AuthIndex: "a1", Model: "claude-sonnet"}

	for i := 0; i < 10; i++ {
		agg.Observe(key, AvailabilitySample{
			At:           now.Add(-time.Duration(10-i) * time.Minute),
			Failed:       i == 3,
			Latency:      2 * time.Second,
			TTFT:         500 * time.Millisecond,
			OutputTokens: 100,
			InputTokens:  50,
			CachedTokens: 10,
		})
	}
	// client fault excluded
	agg.Observe(key, AvailabilitySample{
		At:          now.Add(-time.Minute),
		Failed:      true,
		ClientFault: true,
	})

	snap, ok := agg.SnapshotForKey(key, now, time.Hour, 12)
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.Total != 10 {
		t.Fatalf("total=%d want 10", snap.Total)
	}
	if snap.Success != 9 || snap.Failed != 1 {
		t.Fatalf("success=%d failed=%d", snap.Success, snap.Failed)
	}
	if snap.Excluded != 1 {
		t.Fatalf("excluded=%d", snap.Excluded)
	}
	if snap.Availability < 0.89 || snap.Availability > 0.91 {
		t.Fatalf("availability=%v", snap.Availability)
	}
	if snap.LatencyP50MS <= 0 || snap.TTFTP50MS <= 0 {
		t.Fatalf("latency/ttft missing: %+v", snap)
	}
	if !snap.HasTraffic || snap.Status == "no_data" {
		t.Fatalf("status=%s", snap.Status)
	}
	if len(snap.Buckets) != 12 {
		t.Fatalf("buckets=%d", len(snap.Buckets))
	}
}

func TestAvailabilityPluginFromRecord(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Now()
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "gpt-4o",
		RequestedAt: now,
		Latency:     time.Second,
		TTFT:        200 * time.Millisecond,
		Failed:      true,
		Fail:        Failure{StatusCode: 429},
		Detail:      Detail{OutputTokens: 10, InputTokens: 20},
	})
	snap, ok := agg.SnapshotForKey(AvailabilityKey{
		Provider:  "openai-compatibility",
		AuthIndex: "pool-1",
		Model:     "gpt-4o",
	}, now.Add(time.Second), time.Hour, 24)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snap.RateLimited != 1 || snap.Failed != 1 {
		t.Fatalf("rateLimited=%d failed=%d", snap.RateLimited, snap.Failed)
	}
}
