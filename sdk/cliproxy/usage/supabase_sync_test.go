package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildSupabaseUsageDeltaUsesCanonicalTokenBreakdown(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	detail := Detail{
		InputTokens:         10,
		OutputTokens:        5,
		CachedTokens:        99,
		CacheReadTokens:     4,
		CacheCreationTokens: 6,
		TotalTokens:         25,
	}
	detail.TokenBreakdown = NewIndependentTokenBreakdown(10, 4, 6, 5, 0, 25)
	delta, ok := buildSupabaseUsageDelta(Record{
		Provider:    "claude",
		AuthID:      "auth-secret-id",
		Model:       "claude-sonnet",
		Protocol:    "messages",
		RequestedAt: now,
		Latency:     2 * time.Second,
		TTFT:        500 * time.Millisecond,
		Detail:      detail,
	}, []byte("hash-key"))
	if !ok {
		t.Fatal("expected a usage delta")
	}
	if delta.InputTokens != 20 || delta.CachedTokens != 4 || delta.TotalTokens != 25 {
		t.Fatalf("token delta = input:%d cached:%d total:%d", delta.InputTokens, delta.CachedTokens, delta.TotalTokens)
	}
	if delta.Protocol != "messages" {
		t.Fatalf("protocol = %q, want messages", delta.Protocol)
	}
	if delta.AuthHash == "auth-secret-id" || delta.AuthHash == "" {
		t.Fatalf("auth hash was not stabilized: %q", delta.AuthHash)
	}

	detailJSON, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	if strings.Contains(string(detailJSON), "auth-secret-id") {
		t.Fatalf("raw auth identifier leaked into delta: %s", detailJSON)
	}
}

func TestSupabaseUsageDeltaAggregatesCounters(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	first, ok := buildSupabaseUsageDelta(Record{
		Provider:    "openai",
		AuthIndex:   "a1",
		Model:       "gpt-5",
		Protocol:    "chat",
		RequestedAt: now,
		Latency:     time.Second,
		TTFT:        200 * time.Millisecond,
		Detail:      Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedTokens: 3},
	}, []byte("hash-key"))
	if !ok {
		t.Fatal("expected first delta")
	}
	second, ok := buildSupabaseUsageDelta(Record{
		Provider:    "openai",
		AuthIndex:   "a1",
		Model:       "gpt-5",
		Protocol:    "chat",
		RequestedAt: now.Add(time.Second),
		Latency:     2 * time.Second,
		Failed:      true,
		Fail:        Failure{StatusCode: http.StatusTooManyRequests},
		Detail:      Detail{InputTokens: 20, OutputTokens: 8, TotalTokens: 28, CachedTokens: 2},
	}, []byte("hash-key"))
	if !ok {
		t.Fatal("expected second delta")
	}
	merged := mergeSupabaseUsageDelta(first, second)
	if merged.Requests != 2 || merged.Success != 1 || merged.Failed != 1 || merged.RateLimited != 1 {
		t.Fatalf("request counters = %+v", merged)
	}
	if merged.InputTokens != 30 || merged.OutputTokens != 13 || merged.TotalTokens != 43 || merged.CachedTokens != 5 {
		t.Fatalf("token counters = input:%d output:%d total:%d cached:%d", merged.InputTokens, merged.OutputTokens, merged.TotalTokens, merged.CachedTokens)
	}
	if merged.LatencyCount != 2 || merged.TTFTCount != 1 {
		t.Fatalf("latency counters = latency:%d ttft:%d", merged.LatencyCount, merged.TTFTCount)
	}
}

func TestSupabaseUsageSyncOutboxIsBounded(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "outbox.json")
	syncer, err := NewSupabaseUsageSync(SupabaseUsageSyncConfig{
		URL:              "https://example.supabase.co",
		ServiceRoleKey:   "server-key",
		AuthHashKey:      []byte("hash-key"),
		OutboxPath:       outboxPath,
		MaxOutboxBytes:   8000,
		MaxOutboxBatches: 2,
	})
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := syncer.enqueueBatch(testSupabaseBatch(i)); err != nil {
			t.Fatalf("enqueue batch %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(outboxPath)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if int64(len(data)) > syncer.maxOutboxBytes {
		t.Fatalf("outbox bytes = %d, limit %d", len(data), syncer.maxOutboxBytes)
	}
	var state supabaseUsageOutbox
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode outbox: %v", err)
	}
	if len(state.Batches) > syncer.maxOutboxBatches {
		t.Fatalf("outbox batches = %d, limit %d", len(state.Batches), syncer.maxOutboxBatches)
	}
}

func TestSupabaseUsageSyncRetriesSameIdempotencyKey(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     int
		keys      []string
		rowCounts []int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Key  string            `json:"p_idempotency_key"`
			Rows []usageDailyDelta `json:"p_rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls++
		keys = append(keys, payload.Key)
		rowCounts = append(rowCounts, len(payload.Rows))
		call := calls
		mu.Unlock()
		if call == 1 {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	syncer, err := NewSupabaseUsageSync(SupabaseUsageSyncConfig{
		URL:              server.URL,
		ServiceRoleKey:   "server-key",
		AuthHashKey:      []byte("hash-key"),
		OutboxPath:       filepath.Join(t.TempDir(), "outbox.json"),
		FlushInterval:    10 * time.Millisecond,
		MaxOutboxBytes:   1 << 20,
		MaxOutboxBatches: 10,
	})
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	syncer.Start(ctx)
	syncer.HandleUsage(context.Background(), Record{
		Provider:    "synthetic",
		AuthID:      "auth-1",
		Model:       "model-1",
		Protocol:    "chat",
		RequestedAt: time.Now(),
		Detail:      Detail{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, CachedTokens: 4},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		gotCalls := calls
		gotKeys := append([]string(nil), keys...)
		gotRows := append([]int(nil), rowCounts...)
		mu.Unlock()
		if gotCalls >= 2 {
			if gotKeys[0] == "" || gotKeys[0] != gotKeys[1] {
				t.Fatalf("retry idempotency keys = %#v", gotKeys)
			}
			if gotRows[0] != 1 || gotRows[1] != 1 {
				t.Fatalf("retry row counts = %#v", gotRows)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("expected retry, got %d calls", gotCalls)
	}
	cancel()
	syncer.Close()
}

func testSupabaseBatch(index int) supabaseUsageBatch {
	return supabaseUsageBatch{
		IdempotencyKey: "batch-" + string(rune('0'+index)),
		SchemaVersion:  SupabaseUsageSchemaVersion,
		OccurredAt:     "2026-08-02T12:00:00Z",
		PayloadSHA256:  "hash-" + string(rune('0'+index)),
		Rows: []usageDailyDelta{{
			Day:           "2026-08-02",
			Provider:      "synthetic",
			AuthHash:      "auth-hash",
			Model:         "model",
			Protocol:      "chat",
			OccurredAt:    "2026-08-02T12:00:00Z",
			SchemaVersion: SupabaseUsageSchemaVersion,
			Requests:      1,
		}},
	}
}
