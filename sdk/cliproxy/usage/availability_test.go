package usage

import (
	"context"
	"net/http"
	"path/filepath"
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
			TotalTokens:  150,
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
	if snap.InputTokens != 500 || snap.TotalTokens != 1500 || snap.CachedTokens != 100 {
		t.Fatalf("tokens input=%d total=%d cached=%d", snap.InputTokens, snap.TotalTokens, snap.CachedTokens)
	}
	if snap.CacheHitRate < 0.19 || snap.CacheHitRate > 0.21 {
		t.Fatalf("cache hit rate=%v", snap.CacheHitRate)
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
		Detail:      Detail{OutputTokens: 10, InputTokens: 20, TotalTokens: 30, CachedTokens: 5, CacheReadTokens: 5},
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
	if snap.InputTokens != 20 || snap.TotalTokens != 30 || snap.CachedTokens != 5 {
		t.Fatalf("tokens input=%d total=%d cached=%d", snap.InputTokens, snap.TotalTokens, snap.CachedTokens)
	}
}

func TestAvailabilityPluginCountsNetworkErrors(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Now()
	// Transport-level failure: no HTTP status, flagged as network error.
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-net",
		Model:       "gpt-4o",
		RequestedAt: now,
		Latency:     500 * time.Millisecond,
		Failed:      true,
		Fail:        Failure{NetworkError: true, Body: "dial tcp: i/o timeout"},
	})
	// Rate-limited failure: must NOT count as a network error.
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-net",
		Model:       "gpt-4o",
		RequestedAt: now,
		Latency:     time.Second,
		Failed:      true,
		Fail:        Failure{StatusCode: 429},
	})
	snap, ok := agg.SnapshotForKey(AvailabilityKey{
		Provider:  "openai-compatibility",
		AuthIndex: "pool-net",
		Model:     "gpt-4o",
	}, now.Add(time.Second), time.Hour, 24)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snap.Failed != 2 {
		t.Fatalf("failed=%d, want 2", snap.Failed)
	}
	if snap.NetworkErrors != 1 {
		t.Fatalf("networkErrors=%d, want 1", snap.NetworkErrors)
	}
	if snap.RateLimited != 1 {
		t.Fatalf("rateLimited=%d, want 1", snap.RateLimited)
	}
}

func TestAvailabilityUsesCanonicalInputForCacheRate(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Now()
	detail := Detail{
		InputTokens:         10,
		OutputTokens:        5,
		CacheReadTokens:     4,
		CacheCreationTokens: 6,
		TotalTokens:         25,
	}
	detail.TokenBreakdown = NewIndependentTokenBreakdown(10, 4, 6, 5, 0, 25)
	agg.HandleUsage(context.Background(), Record{
		Provider:    "claude",
		AuthIndex:   "a1",
		Model:       "claude-sonnet",
		RequestedAt: now,
		Detail:      detail,
	})
	snap, ok := agg.SnapshotForKey(AvailabilityKey{Provider: "claude", AuthIndex: "a1", Model: "claude-sonnet"}, now, time.Hour, 1)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snap.InputTokens != 20 || snap.CachedTokens != 4 {
		t.Fatalf("canonical input=%d cached=%d", snap.InputTokens, snap.CachedTokens)
	}
	if snap.CacheHitRate < 0.19 || snap.CacheHitRate > 0.21 {
		t.Fatalf("cache hit rate=%v", snap.CacheHitRate)
	}
}

func TestAvailabilityAggregatorPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "availability-state.json")
	now := time.Now()
	key := AvailabilityKey{Provider: "openai-compatible-火山", AuthIndex: "a1", Model: "glm-5.2"}

	src := NewAvailabilityAggregator(24*time.Hour, 5*time.Minute)
	src.Observe(key, AvailabilitySample{At: now.Add(-time.Minute), Latency: time.Second})
	// Expired sample must be dropped on load.
	src.Observe(key, AvailabilitySample{At: now.Add(-48 * time.Hour), Latency: time.Second})
	if !src.consumeDirty() {
		t.Fatal("expected dirty flag after Observe")
	}
	if src.consumeDirty() {
		t.Fatal("dirty flag should reset after consume")
	}
	if err := src.SaveToFile(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	dst := NewAvailabilityAggregator(24*time.Hour, 5*time.Minute)
	if err := dst.LoadFromFile(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	snap, ok := dst.SnapshotForKey(key, now, time.Hour, 12)
	if !ok {
		t.Fatal("expected snapshot after load")
	}
	if snap.Total != 1 {
		t.Fatalf("total=%d want 1 (expired sample must be dropped)", snap.Total)
	}
	if snap.Key.Provider != key.Provider {
		t.Fatalf("provider=%q want %q", snap.Key.Provider, key.Provider)
	}

	// Loading a missing file is a no-op.
	if err := dst.LoadFromFile(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("load missing: %v", err)
	}
}

func TestAvailabilityProviderCompatibility400CountsAsFailure(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Now()
	// A 400 that reflects a provider capability/compatibility problem must NOT
	// be excluded as a client fault; it counts against availability.
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "deepseek",
		RequestedAt: now,
		Latency:     time.Second,
		Failed:      true,
		Fail:        Failure{StatusCode: http.StatusBadRequest, Body: `{"error":{"type":"invalid_request_error","message":"'type' must be in [\"enabled\", \"disabled\", \"auto\"]"}}`},
	})
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "deepseek",
		RequestedAt: now,
		Latency:     time.Second,
		Failed:      true,
		Fail:        Failure{StatusCode: http.StatusBadRequest, Body: "Error from provider: Upstream request failed: Model is unavailable."},
	})
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "deepseek",
		RequestedAt: now,
		Latency:     time.Second,
		Failed:      false,
	})

	snap, ok := agg.SnapshotForKey(AvailabilityKey{
		Provider:  "openai-compatibility",
		AuthIndex: "pool-1",
		Model:     "deepseek",
	}, now.Add(time.Second), time.Hour, 24)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snap.Total != 3 {
		t.Fatalf("total=%d want 3 (compatibility 400s must be counted)", snap.Total)
	}
	if snap.Failed != 2 || snap.Success != 1 {
		t.Fatalf("failed=%d success=%d want 2/1", snap.Failed, snap.Success)
	}
	if snap.Excluded != 0 {
		t.Fatalf("excluded=%d want 0", snap.Excluded)
	}
	if snap.Availability >= 0.99 {
		t.Fatalf("availability=%v want < 0.99 (compatibility failures must drag it down)", snap.Availability)
	}
}

func TestAvailabilityPlain400StillExcludedAsClientFault(t *testing.T) {
	agg := NewAvailabilityAggregator(time.Hour, 5*time.Minute)
	now := time.Now()
	// A plain request-shape 400 (no provider capability markers) stays excluded.
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "deepseek",
		RequestedAt: now,
		Failed:      true,
		Fail:        Failure{StatusCode: http.StatusBadRequest, Body: `{"error":{"type":"invalid_request_error","message":"invalid value for field messages[1]"}}`},
	})
	agg.HandleUsage(context.Background(), Record{
		Provider:    "openai-compatibility",
		AuthIndex:   "pool-1",
		Model:       "deepseek",
		RequestedAt: now,
		Failed:      false,
	})

	snap, ok := agg.SnapshotForKey(AvailabilityKey{
		Provider:  "openai-compatibility",
		AuthIndex: "pool-1",
		Model:     "deepseek",
	}, now.Add(time.Second), time.Hour, 24)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snap.Total != 1 || snap.Success != 1 || snap.Failed != 0 {
		t.Fatalf("total=%d success=%d failed=%d want 1/1/0", snap.Total, snap.Success, snap.Failed)
	}
	if snap.Excluded != 1 {
		t.Fatalf("excluded=%d want 1", snap.Excluded)
	}
}
