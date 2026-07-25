package usage

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAvailabilityWindow is the primary aggregation window for card metrics.
	DefaultAvailabilityWindow = time.Hour
	// DefaultAvailabilityBucket is the sparkline bucket size (24 buckets → 2h when window is 2h).
	DefaultAvailabilityBucket = 5 * time.Minute
	// DefaultAvailabilityRetention keeps raw samples long enough for 24h series.
	DefaultAvailabilityRetention = 24 * time.Hour
	// maxSamplesPerKey bounds memory for hot keys.
	maxSamplesPerKey = 4096
)

// AvailabilitySample is one observed upstream attempt used for availability metrics.
type AvailabilitySample struct {
	At           time.Time
	Failed       bool
	ClientFault  bool // excluded from availability denominator (cancel / bad request)
	RateLimited  bool
	Latency      time.Duration
	TTFT         time.Duration
	OutputTokens int64
	CachedTokens int64
	InputTokens  int64
}

// AvailabilityKey identifies a channel/auth × model metric series.
type AvailabilityKey struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Source    string `json:"source,omitempty"`
	// Supplier is an optional operator-defined credential group (bulk import).
	Supplier string `json:"supplier,omitempty"`
	Model    string `json:"model"`
	Alias    string `json:"alias,omitempty"`
	// Protocols is the configured upstream protocol set for this channel/model
	// (chat/responses/messages). Empty means not user-customized / default chat-only.
	Protocols []string `json:"protocols,omitempty"`
}

func (k AvailabilityKey) normalize() AvailabilityKey {
	k.Provider = strings.TrimSpace(k.Provider)
	k.AuthID = strings.TrimSpace(k.AuthID)
	k.AuthIndex = strings.TrimSpace(k.AuthIndex)
	k.Source = strings.TrimSpace(k.Source)
	k.Supplier = strings.TrimSpace(k.Supplier)
	k.Model = strings.TrimSpace(k.Model)
	k.Alias = strings.TrimSpace(k.Alias)
	if k.Model == "" {
		k.Model = "unknown"
	}
	return k
}

func (k AvailabilityKey) id() string {
	k = k.normalize()
	channel := k.AuthIndex
	if channel == "" {
		channel = k.AuthID
	}
	if channel == "" {
		channel = k.Provider
	}
	return strings.Join([]string{k.Provider, channel, k.Model}, "\x1f")
}

// AvailabilityBucket is one fixed-size time bucket for sparklines.
type AvailabilityBucket struct {
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Total        int64     `json:"total"`
	Success      int64     `json:"success"`
	Failed       int64     `json:"failed"`
	RateLimited  int64     `json:"rate_limited"`
	SuccessRate  float64   `json:"success_rate"`
	AvgLatencyMS float64   `json:"avg_latency_ms,omitempty"`
}

// AvailabilitySnapshot is aggregated metrics for one key over a window.
type AvailabilitySnapshot struct {
	Key          AvailabilityKey      `json:"key"`
	Window       string               `json:"window"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	Total        int64                `json:"total"`
	Success      int64                `json:"success"`
	Failed       int64                `json:"failed"`
	RateLimited  int64                `json:"rate_limited"`
	Excluded     int64                `json:"excluded_client_faults"`
	SuccessRate  float64              `json:"success_rate"`
	Availability float64              `json:"availability"` // success / (success+failed excluding client faults & optional rate limits as soft)
	LatencyP50MS float64              `json:"latency_p50_ms"`
	LatencyP95MS float64              `json:"latency_p95_ms"`
	TTFTP50MS    float64              `json:"ttft_p50_ms"`
	TTFTP95MS    float64              `json:"ttft_p95_ms"`
	TPS          float64              `json:"tps"`
	CacheHitRate float64              `json:"cache_hit_rate"`
	HasTraffic   bool                 `json:"has_traffic"`
	Buckets      []AvailabilityBucket `json:"buckets,omitempty"`
	LastSampleAt time.Time            `json:"last_sample_at,omitempty"`
	LastErrorAt  time.Time            `json:"last_error_at,omitempty"`
	Status       string               `json:"status"` // running | degraded | no_data
}

// AvailabilityAggregator accumulates usage samples in memory for management views.
type AvailabilityAggregator struct {
	mu        sync.RWMutex
	samples   map[string][]AvailabilitySample
	keys      map[string]AvailabilityKey
	retention time.Duration
	bucket    time.Duration
}

// NewAvailabilityAggregator creates an in-memory aggregator.
func NewAvailabilityAggregator(retention, bucket time.Duration) *AvailabilityAggregator {
	if retention <= 0 {
		retention = DefaultAvailabilityRetention
	}
	if bucket <= 0 {
		bucket = DefaultAvailabilityBucket
	}
	return &AvailabilityAggregator{
		samples:   make(map[string][]AvailabilitySample),
		keys:      make(map[string]AvailabilityKey),
		retention: retention,
		bucket:    bucket,
	}
}

// HandleUsage implements Plugin so the aggregator can register on the usage manager.
func (a *AvailabilityAggregator) HandleUsage(_ context.Context, record Record) {
	if a == nil {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	status := record.Fail.StatusCode
	clientFault := !record.Failed && false
	if record.Failed {
		// Client cancels / bad requests should not drag availability.
		if status == 400 || status == 404 || status == 413 || status == 422 {
			clientFault = true
		}
		// Treat empty body client aborts similarly when status is 0 and latency is tiny? keep simple.
	}
	rateLimited := record.Failed && (status == 429 || status == 402)
	a.Observe(AvailabilityKey{
		Provider:  record.Provider,
		AuthID:    record.AuthID,
		AuthIndex: record.AuthIndex,
		Source:    record.Source,
		Supplier:  record.Supplier,
		Model:     record.Model,
		Alias:     record.Alias,
	}, AvailabilitySample{
		At:           at,
		Failed:       record.Failed,
		ClientFault:  clientFault,
		RateLimited:  rateLimited,
		Latency:      record.Latency,
		TTFT:         record.TTFT,
		OutputTokens: record.Detail.OutputTokens,
		CachedTokens: record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		InputTokens:  record.Detail.InputTokens,
	})
}

// Observe records one sample.
func (a *AvailabilityAggregator) Observe(key AvailabilityKey, sample AvailabilitySample) {
	if a == nil {
		return
	}
	key = key.normalize()
	id := key.id()
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys[id] = key
	list := append(a.samples[id], sample)
	cutoff := sample.At.Add(-a.retention)
	// Drop old samples and cap length.
	if len(list) > 0 {
		start := 0
		for start < len(list) && list[start].At.Before(cutoff) {
			start++
		}
		if start > 0 {
			list = append([]AvailabilitySample(nil), list[start:]...)
		}
		if len(list) > maxSamplesPerKey {
			list = append([]AvailabilitySample(nil), list[len(list)-maxSamplesPerKey:]...)
		}
	}
	a.samples[id] = list
}

// Snapshot returns aggregated metrics for all keys in the window ending at now.
func (a *AvailabilityAggregator) Snapshot(now time.Time, window time.Duration, bucketCount int) []AvailabilitySnapshot {
	if a == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 {
		window = DefaultAvailabilityWindow
	}
	if bucketCount <= 0 {
		bucketCount = 24
	}
	from := now.Add(-window)

	a.mu.RLock()
	defer a.mu.RUnlock()

	out := make([]AvailabilitySnapshot, 0, len(a.keys))
	for id, key := range a.keys {
		samples := a.samples[id]
		snap := buildSnapshot(key, samples, from, now, a.bucket, bucketCount, window)
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		if out[i].Key.Provider != out[j].Key.Provider {
			return out[i].Key.Provider < out[j].Key.Provider
		}
		if out[i].Key.AuthIndex != out[j].Key.AuthIndex {
			return out[i].Key.AuthIndex < out[j].Key.AuthIndex
		}
		return out[i].Key.Model < out[j].Key.Model
	})
	return out
}

// SnapshotForKey returns one key's snapshot.
// EmptySnapshot returns a no-traffic snapshot for a registered channel/model.
func EmptySnapshot(key AvailabilityKey, now time.Time, window time.Duration, bucketCount int) AvailabilitySnapshot {
	key = key.normalize()
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 {
		window = DefaultAvailabilityWindow
	}
	if bucketCount <= 0 {
		bucketCount = 24
	}
	from := now.Add(-window)
	buckets := make([]AvailabilityBucket, bucketCount)
	bucketSize := DefaultAvailabilityBucket
	for i := 0; i < bucketCount; i++ {
		start := now.Add(-time.Duration(bucketCount-i) * bucketSize)
		buckets[i] = AvailabilityBucket{Start: start, End: start.Add(bucketSize)}
	}
	return AvailabilitySnapshot{
		Key:        key,
		Window:     window.String(),
		From:       from,
		To:         now,
		Buckets:    buckets,
		Status:     "no_data",
		HasTraffic: false,
	}
}

func (a *AvailabilityAggregator) SnapshotForKey(key AvailabilityKey, now time.Time, window time.Duration, bucketCount int) (AvailabilitySnapshot, bool) {
	if a == nil {
		return AvailabilitySnapshot{}, false
	}
	key = key.normalize()
	id := key.id()
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 {
		window = DefaultAvailabilityWindow
	}
	if bucketCount <= 0 {
		bucketCount = 24
	}
	from := now.Add(-window)
	a.mu.RLock()
	defer a.mu.RUnlock()
	samples, ok := a.samples[id]
	if !ok {
		return AvailabilitySnapshot{}, false
	}
	storedKey := a.keys[id]
	return buildSnapshot(storedKey, samples, from, now, a.bucket, bucketCount, window), true
}

func buildSnapshot(key AvailabilityKey, samples []AvailabilitySample, from, to time.Time, bucketSize time.Duration, bucketCount int, window time.Duration) AvailabilitySnapshot {
	if bucketSize <= 0 {
		bucketSize = DefaultAvailabilityBucket
	}
	// Align bucket window to end at `to`.
	bucketWindow := time.Duration(bucketCount) * bucketSize
	bucketStart := to.Add(-bucketWindow)
	if bucketStart.Before(from) {
		// still produce bucketCount buckets ending at `to`
	}

	buckets := make([]AvailabilityBucket, bucketCount)
	for i := 0; i < bucketCount; i++ {
		start := to.Add(-time.Duration(bucketCount-i) * bucketSize)
		buckets[i] = AvailabilityBucket{Start: start, End: start.Add(bucketSize)}
	}

	var (
		total, success, failed, rateLimited, excluded int64
		latencies, ttfts                              []float64
		outTokens                                     float64
		genSeconds                                    float64
		cached, inputs                                int64
		lastSample, lastError                         time.Time
		bucketLatencySum                              = make([]float64, bucketCount)
		bucketLatencyN                                = make([]int64, bucketCount)
	)

	for _, s := range samples {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		if s.At.After(lastSample) {
			lastSample = s.At
		}
		if s.ClientFault {
			excluded++
			continue
		}
		total++
		if s.Failed {
			failed++
			if s.At.After(lastError) {
				lastError = s.At
			}
			if s.RateLimited {
				rateLimited++
			}
		} else {
			success++
		}
		if s.Latency > 0 {
			latencies = append(latencies, float64(s.Latency)/float64(time.Millisecond))
		}
		if s.TTFT > 0 {
			ttfts = append(ttfts, float64(s.TTFT)/float64(time.Millisecond))
		}
		if s.OutputTokens > 0 && s.Latency > 0 {
			// Prefer generation time after first token when available.
			gen := s.Latency
			if s.TTFT > 0 && s.Latency > s.TTFT {
				gen = s.Latency - s.TTFT
			}
			if gen > 0 {
				outTokens += float64(s.OutputTokens)
				genSeconds += gen.Seconds()
			}
		}
		if s.InputTokens > 0 {
			inputs += s.InputTokens
		}
		if s.CachedTokens > 0 {
			cached += s.CachedTokens
		}

		// Sparkline buckets.
		if !s.At.Before(bucketStart) {
			idx := int(s.At.Sub(bucketStart) / bucketSize)
			if idx < 0 {
				idx = 0
			}
			if idx >= bucketCount {
				idx = bucketCount - 1
			}
			b := &buckets[idx]
			b.Total++
			if s.Failed {
				b.Failed++
				if s.RateLimited {
					b.RateLimited++
				}
			} else {
				b.Success++
			}
			if s.Latency > 0 {
				bucketLatencySum[idx] += float64(s.Latency) / float64(time.Millisecond)
				bucketLatencyN[idx]++
			}
		}
	}

	for i := range buckets {
		if buckets[i].Total > 0 {
			buckets[i].SuccessRate = float64(buckets[i].Success) / float64(buckets[i].Total)
		}
		if bucketLatencyN[i] > 0 {
			buckets[i].AvgLatencyMS = bucketLatencySum[i] / float64(bucketLatencyN[i])
		}
	}

	availDenom := success + failed
	// Rate limits count as failed for availability but are exposed separately.
	successRate := 0.0
	availability := 0.0
	if availDenom > 0 {
		successRate = float64(success) / float64(availDenom)
		availability = successRate
	}

	tps := 0.0
	if genSeconds > 0 {
		tps = outTokens / genSeconds
	}
	cacheHit := 0.0
	if inputs > 0 {
		cacheHit = float64(cached) / float64(inputs)
		if cacheHit > 1 {
			cacheHit = 1
		}
	}

	status := "no_data"
	if total > 0 {
		status = "running"
		if availability < 0.95 || rateLimited > 0 {
			status = "degraded"
		}
	}

	return AvailabilitySnapshot{
		Key:          key,
		Window:       window.String(),
		From:         from,
		To:           to,
		Total:        total,
		Success:      success,
		Failed:       failed,
		RateLimited:  rateLimited,
		Excluded:     excluded,
		SuccessRate:  round4(successRate),
		Availability: round4(availability),
		LatencyP50MS: round2(percentile(latencies, 0.50)),
		LatencyP95MS: round2(percentile(latencies, 0.95)),
		TTFTP50MS:    round2(percentile(ttfts, 0.50)),
		TTFTP95MS:    round2(percentile(ttfts, 0.95)),
		TPS:          round2(tps),
		CacheHitRate: round4(cacheHit),
		HasTraffic:   total > 0,
		Buckets:      buckets,
		LastSampleAt: lastSample,
		LastErrorAt:  lastError,
		Status:       status,
	}
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 1 {
		return cp[len(cp)-1]
	}
	idx := p * float64(len(cp)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return cp[lo]
	}
	w := idx - float64(lo)
	return cp[lo]*(1-w) + cp[hi]*w
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

var (
	defaultAvailability = NewAvailabilityAggregator(DefaultAvailabilityRetention, DefaultAvailabilityBucket)
	registerAvailOnce   sync.Once
)

// DefaultAvailabilityAggregator returns the process-wide aggregator.
func DefaultAvailabilityAggregator() *AvailabilityAggregator {
	return defaultAvailability
}

// EnsureAvailabilityPluginRegistered registers the default aggregator on the usage manager once.
func EnsureAvailabilityPluginRegistered() {
	registerAvailOnce.Do(func() {
		RegisterNamedPlugin("availability", defaultAvailability)
	})
}
