package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// UsageStream is the control-plane usage event stream the metering worker
// consumes (same name as the gateway/quota publisher).
const UsageStream = "stream:odu.usage.recorded"

// usageEvent mirrors contract event.Event: odu.usage.recorded payloads.
type usageEvent struct {
	EventID    string         `json:"event_id"`
	EventType  string         `json:"event_type"`
	OccurredAt string         `json:"occurred_at"`
	Producer   string         `json:"producer"`
	Data       map[string]any `json:"data"`
}

// UsagePlugin forwards CPA usage records (token detail from upstream
// responses) into the same event stream metering consumes, tagged with the
// control-plane key's project/tenant attribution. Keys without a control-plane
// snapshot (local/static api-keys mode) are skipped — they never hit our
// billing books.
type UsagePlugin struct {
	RDB *redis.Client
}

func actualUsageTokens(detail usage.Detail) int64 {
	return usage.EnsureTokenBreakdown(detail).TotalTokens
}

func settleTPM(ctx context.Context, rdb *redis.Client, snap *Snapshot, kh string, detail usage.Detail) {
	if rdb == nil || snap == nil || snap.TPM <= 0 || strings.TrimSpace(kh) == "" {
		return
	}
	tokens := actualUsageTokens(detail)
	if tokens <= 0 {
		return
	}
	key := tpmUsageKey(kh)
	used, err := rdb.IncrBy(ctx, key, tokens).Result()
	if err != nil {
		log.Printf("controlplane-usage: tpm settlement failed: %v", err)
		return
	}
	if used == tokens {
		if errExpire := rdb.Expire(ctx, key, time.Minute).Err(); errExpire != nil {
			log.Printf("controlplane-usage: tpm settlement ttl failed: %v", errExpire)
		}
	}
}

// HandleUsage implements usage.Plugin.
func (p *UsagePlugin) HandleUsage(ctx context.Context, record usage.Record) {
	log.Printf("controlplane-usage: snap=%v model=%s alias=%s failed=%v", KeySnapshot(ctx) != nil, record.Model, record.Alias, record.Failed)
	if p.RDB == nil {
		return
	}
	snap := KeySnapshot(ctx)
	if snap == nil {
		return
	}
	xctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// TPM is settled from the provider-reported usage after the response has
	// completed. There is intentionally no request-time token reservation.
	settleTPM(xctx, p.RDB, snap, keyHashFromContext(ctx), record.Detail)
	status := "success"
	if record.Failed {
		status = "failed"
	}
	modelServed := record.Model
	if modelServed == "" {
		modelServed = record.Alias
	}
	ts := time.Now().UTC()
	evt := usageEvent{
		EventID:    fmt.Sprintf("cpa-%d", ts.UnixNano()),
		EventType:  "odu.usage.recorded",
		OccurredAt: ts.Format(time.RFC3339Nano),
		Producer:   "cpa-gateway",
		Data: map[string]any{
			"request_id":      fmt.Sprintf("cpa-%d", ts.UnixNano()),
			"attempt_no":      1,
			"status":          status,
			"tenant_id":       snap.TenantID,
			"project_id":      snap.ProjectID,
			"consumer_id":     defaultString(snap.ConsumerID, "default"),
			"api_key_id":      snap.KeyID,
			"user_id":         nil,
			"provider":        record.Provider,
			"model_requested": record.Alias,
			"model_served":    modelServed,
			"is_stream":       false,
			"input_tokens":    record.Detail.InputTokens,
			"output_tokens":   record.Detail.OutputTokens,
			"cached_tokens":   record.Detail.CachedTokens,
			"usage_source":    "exact",
			"duration_ms":     record.Latency.Milliseconds(),
			"ttft_ms":         ttftPtr(record.TTFT),
			"error_code":      nil,
			"trace_id":        fmt.Sprintf("cpa-%d", ts.UnixNano()),
			"timestamp":       ts.Format(time.RFC3339Nano),
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return
	}
	// The plugin callback runs after the request completes (the dispatch
	// queue drains post-response), so the request ctx is already canceled.
	res, err := p.RDB.XAdd(xctx, &redis.XAddArgs{
		Stream: UsageStream,
		Values: map[string]any{"payload": string(payload)},
	}).Result()
	if err != nil {
		log.Printf("controlplane-usage: xadd failed: %v", err)
	} else {
		log.Printf("controlplane-usage: xadded id=%s", res)
	}
}

func ttftPtr(d time.Duration) *int64 {
	if d <= 0 {
		return nil
	}
	ms := d.Milliseconds()
	return &ms
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
