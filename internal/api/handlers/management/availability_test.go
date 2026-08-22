package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestParseAvailabilityWindowCapsAtConfiguredRetention(t *testing.T) {
	// Default retention (24h) keeps the historical cap.
	if got := parseAvailabilityWindow("7d"); got != 24*time.Hour {
		t.Fatalf("default window cap = %v, want 24h", got)
	}

	// Widen the retention, then long-range windows become selectable.
	coreusage.ConfigureDefaultAvailability(720*time.Hour, 0)
	defer coreusage.ConfigureDefaultAvailability(coreusage.DefaultAvailabilityRetention, 0)
	if got := parseAvailabilityWindow("7d"); got != 7*24*time.Hour {
		t.Fatalf("7d window = %v, want %v", got, 7*24*time.Hour)
	}
	if got := parseAvailabilityWindow("30d"); got != 720*time.Hour {
		t.Fatalf("30d window = %v, want %v", got, 720*time.Hour)
	}
	// Windows beyond retention are still capped.
	if got := parseAvailabilityWindow("45d"); got != 720*time.Hour {
		t.Fatalf("45d window = %v, want capped 720h", got)
	}
}

func TestAggregateAvailabilityGroupsCountsOnlyModelsWithTraffic(t *testing.T) {
	items := []coreusage.AvailabilitySnapshot{{
		Key:        coreusage.AvailabilityKey{Provider: "xai", Model: "grok-4"},
		Total:      1,
		Success:    1,
		HasTraffic: true,
	}}
	for i := 0; i < 38; i++ {
		items = append(items, coreusage.AvailabilitySnapshot{
			Key: coreusage.AvailabilityKey{Provider: "xai", Model: "idle-model"},
		})
	}

	groups := aggregateAvailabilityGroups(items, nil)
	if len(groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(groups))
	}
	if groups[0].Models != 1 {
		t.Fatalf("models with traffic = %d, want 1", groups[0].Models)
	}
	if groups[0].IdleModels != 38 {
		t.Fatalf("idle models = %d, want 38", groups[0].IdleModels)
	}
}

func TestAggregateAvailabilityGroupsWeightsCacheHitRateByInputTokens(t *testing.T) {
	items := []coreusage.AvailabilitySnapshot{
		{
			Key:          coreusage.AvailabilityKey{Provider: "codex", Model: "high-volume"},
			Total:        100,
			Success:      100,
			InputTokens:  10000,
			TotalTokens:  10000,
			CachedTokens: 9000,
			HasTraffic:   true,
		},
		{
			Key:         coreusage.AvailabilityKey{Provider: "codex", Model: "probe"},
			Total:       3,
			Success:     3,
			InputTokens: 30,
			TotalTokens: 30,
			HasTraffic:  true,
		},
	}

	groups := aggregateAvailabilityGroups(items, nil)
	if len(groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(groups))
	}
	group := groups[0]
	if group.TotalTokens != 10030 || group.CachedTokens != 9000 {
		t.Fatalf("tokens total=%d cached=%d", group.TotalTokens, group.CachedTokens)
	}
	wantRate := float64(9000) / 10030
	if group.CacheHitRate < wantRate-0.0001 || group.CacheHitRate > wantRate+0.0001 {
		t.Fatalf("cache hit rate=%v want %v", group.CacheHitRate, wantRate)
	}
}

func TestReconcileAvailabilitySnapshotsDropsStaleModelAliases(t *testing.T) {
	registered := []coreusage.AvailabilityKey{
		{
			Provider:  "openai-compatible-relay",
			AuthIndex: "auth-1",
			Model:     "gpt-5.6-luna",
		},
	}
	snapshots := []coreusage.AvailabilitySnapshot{
		{
			Key: coreusage.AvailabilityKey{
				Provider:  "openai-compatible-relay",
				AuthIndex: "auth-1",
				Model:     "gpt-5.6-luna",
				Alias:     "deleted-alias",
			},
			Total:      2,
			HasTraffic: true,
		},
		{
			Key: coreusage.AvailabilityKey{
				Provider:  "openai-compatible-relay",
				AuthIndex: "auth-1",
				Model:     "deleted-model",
			},
		},
	}

	got := reconcileAvailabilitySnapshots(snapshots, registered)
	if len(got) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(got))
	}
	if got[0].Key.Alias != "" {
		t.Fatalf("stale alias = %q, want empty", got[0].Key.Alias)
	}
	if got[0].Total != 2 || !got[0].HasTraffic {
		t.Fatalf("historical metrics were not preserved: %+v", got[0])
	}
}

func TestReconcileAvailabilitySnapshotsUsesLiveAlias(t *testing.T) {
	registered := []coreusage.AvailabilityKey{
		{
			Provider:  "openai-compatible-relay",
			AuthIndex: "auth-1",
			Model:     "deepseek-v4",
			Alias:     "gpt-5.6-luna",
		},
	}
	snapshots := []coreusage.AvailabilitySnapshot{{
		Key: coreusage.AvailabilityKey{
			Provider:  "openai-compatible-relay",
			AuthIndex: "auth-1",
			Model:     "deepseek-v4",
			Alias:     "old-alias",
		},
	}}

	got := reconcileAvailabilitySnapshots(snapshots, registered)
	if len(got) != 1 || got[0].Key.Alias != "gpt-5.6-luna" {
		t.Fatalf("live alias = %q, want gpt-5.6-luna", got[0].Key.Alias)
	}
}

func TestReconcileAvailabilitySnapshotsMatchesLiveRouteAlias(t *testing.T) {
	registered := []coreusage.AvailabilityKey{
		{
			Provider:  "openai-compatible-relay",
			AuthIndex: "auth-1",
			Model:     "gpt-5.6-luna",
		},
	}
	snapshots := []coreusage.AvailabilitySnapshot{{
		Key: coreusage.AvailabilityKey{
			Provider:  "openai-compatible-relay",
			AuthIndex: "auth-1",
			Model:     "deepseek-v4",
			Alias:     "gpt-5.6-luna",
		},
		Total: 1,
	}}

	got := reconcileAvailabilitySnapshots(snapshots, registered)
	if len(got) != 1 || got[0].Key.Model != "gpt-5.6-luna" || got[0].Key.Alias != "" {
		t.Fatalf("live route was not canonicalized: %+v", got)
	}
}

func TestCompatConfiguredModelsKeepsPerKeyModelSetsSeparate(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "relay",
		BaseURL: "https://relay.example/v1",
		Models: []config.OpenAICompatibilityModel{
			{Name: "shared-default"},
		},
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{
			{APIKey: "key-1", Models: []config.OpenAICompatibilityModel{{Name: "deepseek"}}},
			{APIKey: "key-2", Models: []config.OpenAICompatibilityModel{{Name: "deepseek"}, {Name: "claude"}}},
			{APIKey: "key-3"},
		},
	}}}

	key1 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-1")
	if len(key1) != 1 || key1[0] != "deepseek" {
		t.Fatalf("key-1 models = %#v, want [deepseek]", key1)
	}

	key2 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-2")
	if len(key2) != 2 || key2[0] != "deepseek" || key2[1] != "claude" {
		t.Fatalf("key-2 models = %#v, want [deepseek claude]", key2)
	}

	key3 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-3")
	if len(key3) != 1 || key3[0] != "shared-default" {
		t.Fatalf("key-3 models = %#v, want inherited [shared-default]", key3)
	}
}

func TestMatchOpenAICompatibilityModelProtocolsPrefersExactName(t *testing.T) {
	models := []config.OpenAICompatibilityModel{
		{Name: "deepseek-v4-flash", Alias: "gpt-5.6-luna", Protocols: []string{"chat"}},
		{Name: "gpt-5.6-luna", Protocols: []string{"chat", "responses"}},
	}

	got := matchOpenAICompatibilityModelProtocols(models, "gpt-5.6-luna")
	want := []string{"chat", "responses"}
	if len(got) != len(want) {
		t.Fatalf("protocols = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("protocols[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAggregateAvailabilityGroupsCountsNetworkErrors(t *testing.T) {
	items := []coreusage.AvailabilitySnapshot{
		{
			Key:           coreusage.AvailabilityKey{Provider: "openai", Model: "gpt-5"},
			Total:         4,
			Failed:        2,
			NetworkErrors: 2,
			HasTraffic:    true,
		},
		{
			Key:        coreusage.AvailabilityKey{Provider: "openai", Model: "gpt-4"},
			Total:      2,
			Success:    2,
			HasTraffic: true,
		},
	}

	groups := aggregateAvailabilityGroups(items, nil)
	if len(groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(groups))
	}
	if groups[0].NetworkErrors != 2 {
		t.Fatalf("group network errors = %d, want 2", groups[0].NetworkErrors)
	}
	if groups[0].Failed != 2 || groups[0].Total != 6 {
		t.Fatalf("group failed=%d total=%d", groups[0].Failed, groups[0].Total)
	}
}

func TestAggregateAvailabilityGroupStatusUsesRecentBucketsAndCooldown(t *testing.T) {
	now := time.Now()
	mkSnap := func(provider string, buckets []coreusage.AvailabilityBucket) coreusage.AvailabilitySnapshot {
		var total, success int64
		for _, b := range buckets {
			total += b.Total
			success += b.Success
		}
		return coreusage.AvailabilitySnapshot{
			Key:        coreusage.AvailabilityKey{Provider: provider, Model: "m"},
			Total:      total,
			Success:    success,
			HasTraffic: total > 0,
			Buckets:    buckets,
		}
	}

	items := []coreusage.AvailabilitySnapshot{
		// All healthy → running.
		mkSnap("healthy", recentTestBuckets(now, 10, 10, 0, 0)),
		// Recent buckets carry failures (90% < 95%) → degraded.
		mkSnap("flaky", recentTestBuckets(now, 10, 9, 1, 0)),
		// Recent buckets carry rate limits → cooldown.
		mkSnap("limited", recentTestBuckets(now, 10, 9, 1, 1)),
		// Window traffic but no recent buckets → no_data.
		{
			Key:        coreusage.AvailabilityKey{Provider: "quiet", Model: "m"},
			Total:      5,
			Success:    5,
			HasTraffic: true,
			Buckets:    nil,
		},
	}

	groups := aggregateAvailabilityGroups(items, map[string]bool{"limited": true})
	statusByProvider := map[string]string{}
	for _, g := range groups {
		statusByProvider[g.Provider] = g.Status
	}
	if got := statusByProvider["healthy"]; got != "running" {
		t.Fatalf("healthy status = %q, want running", got)
	}
	if got := statusByProvider["flaky"]; got != "degraded" {
		t.Fatalf("flaky status = %q, want degraded", got)
	}
	if got := statusByProvider["limited"]; got != "cooldown" {
		t.Fatalf("limited status = %q, want cooldown", got)
	}
	if got := statusByProvider["quiet"]; got != "no_data" {
		t.Fatalf("quiet status = %q, want no_data", got)
	}
}

// recentTestBuckets builds n buckets of 5 minutes where recent (last 2) carry
// the given failure/rate-limit counts; earlier buckets are all healthy.
func recentTestBuckets(now time.Time, n int, success int64, failed, rateLimited int64) []coreusage.AvailabilityBucket {
	out := make([]coreusage.AvailabilityBucket, 0, n)
	for i := 0; i < n; i++ {
		b := coreusage.AvailabilityBucket{Start: now.Add(-time.Duration(n-i) * 5 * time.Minute), End: now.Add(-time.Duration(n-i-1) * 5 * time.Minute), Total: success + failed, Success: success, Failed: failed, RateLimited: rateLimited}
		if i < n-2 {
			b.Failed = 0
			b.RateLimited = 0
		}
		out = append(out, b)
	}
	return out
}
