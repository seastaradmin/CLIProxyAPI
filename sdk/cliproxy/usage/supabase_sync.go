package usage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	SupabaseUsageSchemaVersion = 1

	defaultSupabaseUsageFlushInterval = 10 * time.Second
	defaultSupabaseUsageOutboxBytes   = 32 * 1024 * 1024
	defaultSupabaseUsageOutboxBatches = 10000
	defaultSupabaseUsageInputBuffer   = 2048
	defaultSupabaseUsagePendingRows   = 4096
	defaultSupabaseUsageBatchRows     = 512

	supabaseUsageServiceRoleKeyEnv = "CPA_SUPABASE_SERVICE_ROLE_KEY"
	supabaseUsageURLFallbackEnv    = "CPA_SUPABASE_URL"
	supabaseUsageHashKeyEnv        = "CPA_SUPABASE_AUTH_HASH_KEY"
	supabaseUsageOutboxPathEnv     = "CPA_SUPABASE_USAGE_OUTBOX_PATH"
)

// SupabaseUsageSyncConfig configures the server-side Supabase usage sink.
// ServiceRoleKey must only be supplied through the process environment.
type SupabaseUsageSyncConfig struct {
	URL            string
	ServiceRoleKey string
	AuthHashKey    []byte
	OutboxPath     string

	FlushInterval    time.Duration
	MaxOutboxBytes   int64
	MaxOutboxBatches int

	HTTPClient *http.Client
}

// SupabaseUsageSync asynchronously sends bounded daily usage deltas to Supabase.
type SupabaseUsageSync struct {
	endpoint         string
	serviceRoleKey   string
	authHashKey      []byte
	outboxPath       string
	httpClient       *http.Client
	flushInterval    time.Duration
	maxOutboxBytes   int64
	maxOutboxBatches int

	input chan usageDailyDelta

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	started   atomic.Bool
	closed    atomic.Bool

	pendingMu sync.Mutex
	pending   map[usageDailyKey]usageDailyDelta

	outboxMu sync.Mutex
	outbox   supabaseUsageOutbox

	droppedInputBatches  atomic.Uint64
	droppedOutboxBatches atomic.Uint64
}

type usageDailyKey struct {
	Day      string
	Provider string
	Supplier string
	AuthHash string
	Model    string
	Protocol string
}

// usageDailyDelta mirrors the Supabase RPC row and contains no request payload,
// headers, API key, or failure body.
type usageDailyDelta struct {
	Day                    string  `json:"day"`
	Provider               string  `json:"provider"`
	Supplier               string  `json:"supplier"`
	AuthHash               string  `json:"auth_hash"`
	Model                  string  `json:"model"`
	Protocol               string  `json:"protocol"`
	OccurredAt             string  `json:"occurred_at"`
	SchemaVersion          int     `json:"schema_version"`
	Requests               int64   `json:"requests"`
	Success                int64   `json:"success"`
	Failed                 int64   `json:"failed"`
	RateLimited            int64   `json:"rate_limited"`
	ExcludedClientFaults   int64   `json:"excluded_client_faults"`
	InputTokens            int64   `json:"input_tokens"`
	OutputTokens           int64   `json:"output_tokens"`
	TotalTokens            int64   `json:"total_tokens"`
	CachedTokens           int64   `json:"cached_tokens"`
	LatencyMSSum           float64 `json:"latency_ms_sum"`
	LatencyCount           int64   `json:"latency_count"`
	TTFTMSSum              float64 `json:"ttft_ms_sum"`
	TTFTCount              int64   `json:"ttft_count"`
	GenerationOutputTokens int64   `json:"generation_output_tokens"`
	GenerationSeconds      float64 `json:"generation_seconds"`
}

type supabaseUsageBatch struct {
	IdempotencyKey string            `json:"idempotency_key"`
	SchemaVersion  int               `json:"schema_version"`
	OccurredAt     string            `json:"occurred_at"`
	PayloadSHA256  string            `json:"payload_sha256"`
	Rows           []usageDailyDelta `json:"rows"`
}

type supabaseUsageOutbox struct {
	Batches []supabaseUsageBatch `json:"batches"`
}

// NewSupabaseUsageSyncFromEnv creates the sink when both server-side settings
// are present. Missing settings intentionally disable the optional sink.
func NewSupabaseUsageSyncFromEnv(configPath string) (*SupabaseUsageSync, error) {
	url := firstNonEmptyEnv(supabaseUsageURLFallbackEnv, "SUPABASE_URL")
	serviceRoleKey := firstNonEmptyEnv(supabaseUsageServiceRoleKeyEnv, "SUPABASE_SERVICE_ROLE_KEY")
	if url == "" && serviceRoleKey == "" {
		return nil, nil
	}
	if url == "" || serviceRoleKey == "" {
		return nil, fmt.Errorf("supabase usage sync requires both URL and server-side key")
	}

	outboxPath := strings.TrimSpace(os.Getenv(supabaseUsageOutboxPathEnv))
	if outboxPath == "" {
		baseDir := filepath.Dir(configPath)
		if baseDir == "." || baseDir == "" {
			baseDir, _ = os.Getwd()
		}
		outboxPath = filepath.Join(baseDir, "supabase-usage-outbox.json")
	}

	hashKey := []byte(strings.TrimSpace(os.Getenv(supabaseUsageHashKeyEnv)))
	if len(hashKey) == 0 {
		// Reusing the server-side key keeps hashes stable without adding another
		// secret to configuration. The key itself is never serialized or logged.
		hashKey = []byte(serviceRoleKey)
	}

	return NewSupabaseUsageSync(SupabaseUsageSyncConfig{
		URL:              url,
		ServiceRoleKey:   serviceRoleKey,
		AuthHashKey:      hashKey,
		OutboxPath:       outboxPath,
		FlushInterval:    defaultSupabaseUsageFlushInterval,
		MaxOutboxBytes:   defaultSupabaseUsageOutboxBytes,
		MaxOutboxBatches: defaultSupabaseUsageOutboxBatches,
	})
}

// NewSupabaseUsageSync constructs a sink with explicit settings.
func NewSupabaseUsageSync(cfg SupabaseUsageSyncConfig) (*SupabaseUsageSync, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("supabase usage sync: URL is required")
	}
	if strings.TrimSpace(cfg.ServiceRoleKey) == "" {
		return nil, fmt.Errorf("supabase usage sync: server-side key is required")
	}
	if strings.TrimSpace(cfg.OutboxPath) == "" {
		return nil, fmt.Errorf("supabase usage sync: outbox path is required")
	}
	if len(cfg.AuthHashKey) == 0 {
		return nil, fmt.Errorf("supabase usage sync: auth hash key is required")
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultSupabaseUsageFlushInterval
	}
	if cfg.MaxOutboxBytes <= 0 {
		cfg.MaxOutboxBytes = defaultSupabaseUsageOutboxBytes
	}
	if cfg.MaxOutboxBatches <= 0 {
		cfg.MaxOutboxBatches = defaultSupabaseUsageOutboxBatches
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	syncer := &SupabaseUsageSync{
		endpoint:         baseURL + "/rest/v1/rpc/cpa_usage_apply_batch",
		serviceRoleKey:   cfg.ServiceRoleKey,
		authHashKey:      append([]byte(nil), cfg.AuthHashKey...),
		outboxPath:       cfg.OutboxPath,
		httpClient:       cfg.HTTPClient,
		flushInterval:    cfg.FlushInterval,
		maxOutboxBytes:   cfg.MaxOutboxBytes,
		maxOutboxBatches: cfg.MaxOutboxBatches,
		input:            make(chan usageDailyDelta, defaultSupabaseUsageInputBuffer),
		done:             make(chan struct{}),
		pending:          make(map[usageDailyKey]usageDailyDelta),
	}
	if err := syncer.loadOutbox(); err != nil {
		return nil, err
	}
	return syncer, nil
}

// Start registers the sink and starts its background worker.
func (s *SupabaseUsageSync) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		workerCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.started.Store(true)
		RegisterNamedPlugin("supabase_usage_sync", s)
		go s.run(workerCtx)
	})
}

// Close stops the worker and durably stores any pending deltas in the bounded outbox.
func (s *SupabaseUsageSync) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if !s.started.Load() {
			return
		}
		if s.cancel != nil {
			s.cancel()
		}
		<-s.done
	})
}

// HandleUsage implements Plugin. It never performs disk or network I/O.
func (s *SupabaseUsageSync) HandleUsage(_ context.Context, record Record) {
	if s == nil || s.closed.Load() {
		return
	}
	delta, ok := buildSupabaseUsageDelta(record, s.authHashKey)
	if !ok {
		return
	}
	select {
	case s.input <- delta:
	default:
		s.droppedInputBatches.Add(1)
	}
}

func (s *SupabaseUsageSync) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case delta := <-s.input:
			s.addPending(delta)
			if s.pendingSize() >= defaultSupabaseUsageBatchRows {
				s.flushPendingToOutbox()
				s.deliverOutbox(ctx)
			}
		case <-ticker.C:
			s.drainInput()
			s.flushPendingToOutbox()
			s.deliverOutbox(ctx)
		case <-ctx.Done():
			s.drainInput()
			s.flushPendingToOutbox()
			return
		}
	}
}

func (s *SupabaseUsageSync) drainInput() {
	for {
		select {
		case delta := <-s.input:
			s.addPending(delta)
		default:
			return
		}
	}
}

func (s *SupabaseUsageSync) addPending(delta usageDailyDelta) {
	key := usageDailyKey{
		Day:      delta.Day,
		Provider: delta.Provider,
		Supplier: delta.Supplier,
		AuthHash: delta.AuthHash,
		Model:    delta.Model,
		Protocol: delta.Protocol,
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, exists := s.pending[key]; !exists && len(s.pending) >= defaultSupabaseUsagePendingRows {
		s.droppedInputBatches.Add(1)
		return
	}
	s.pending[key] = mergeSupabaseUsageDelta(s.pending[key], delta)
}

func (s *SupabaseUsageSync) pendingSize() int {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return len(s.pending)
}

func (s *SupabaseUsageSync) flushPendingToOutbox() {
	s.pendingMu.Lock()
	if len(s.pending) == 0 {
		s.pendingMu.Unlock()
		return
	}
	rows := make([]usageDailyDelta, 0, len(s.pending))
	for _, row := range s.pending {
		rows = append(rows, row)
	}
	s.pending = make(map[usageDailyKey]usageDailyDelta)
	s.pendingMu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day != rows[j].Day {
			return rows[i].Day < rows[j].Day
		}
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Protocol < rows[j].Protocol
	})

	now := time.Now().UTC()
	rowBytes, err := json.Marshal(rows)
	if err != nil {
		log.WithError(err).Warn("supabase usage sync: failed to encode batch")
		return
	}
	hash := sha256.Sum256(rowBytes)
	batch := supabaseUsageBatch{
		IdempotencyKey: uuid.NewString(),
		SchemaVersion:  SupabaseUsageSchemaVersion,
		OccurredAt:     now.Format(time.RFC3339Nano),
		PayloadSHA256:  hex.EncodeToString(hash[:]),
		Rows:           rows,
	}
	if err := s.enqueueBatch(batch); err != nil {
		log.WithError(err).Warn("supabase usage sync: failed to persist outbox batch")
	}
}

func (s *SupabaseUsageSync) deliverOutbox(ctx context.Context) {
	for {
		batch, ok := s.peekBatch()
		if !ok {
			return
		}
		if err := s.sendBatch(ctx, batch); err != nil {
			log.WithError(err).Warn("supabase usage sync: batch delivery failed; retaining outbox")
			return
		}
		if err := s.ackBatch(batch.IdempotencyKey); err != nil {
			log.WithError(err).Warn("supabase usage sync: batch acknowledged remotely but outbox cleanup failed")
			return
		}
	}
}

func (s *SupabaseUsageSync) sendBatch(ctx context.Context, batch supabaseUsageBatch) error {
	body, err := json.Marshal(map[string]any{
		"p_idempotency_key": batch.IdempotencyKey,
		"p_schema_version":  batch.SchemaVersion,
		"p_occurred_at":     batch.OccurredAt,
		"p_payload_sha256":  batch.PayloadSHA256,
		"p_rows":            batch.Rows,
	})
	if err != nil {
		return fmt.Errorf("encode RPC request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", s.serviceRoleKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceRoleKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call RPC: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Debug("supabase usage sync: response body close failed")
		}
	}()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("RPC returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *SupabaseUsageSync) loadOutbox() error {
	data, err := os.ReadFile(s.outboxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read outbox: %w", err)
	}
	var state supabaseUsageOutbox
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode outbox: %w", err)
	}
	s.outbox = state
	return nil
}

func (s *SupabaseUsageSync) peekBatch() (supabaseUsageBatch, bool) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if len(s.outbox.Batches) == 0 {
		return supabaseUsageBatch{}, false
	}
	return s.outbox.Batches[0], true
}

func (s *SupabaseUsageSync) ackBatch(idempotencyKey string) error {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if len(s.outbox.Batches) == 0 || s.outbox.Batches[0].IdempotencyKey != idempotencyKey {
		return nil
	}
	next := supabaseUsageOutbox{Batches: append([]supabaseUsageBatch(nil), s.outbox.Batches[1:]...)}
	if err := s.writeOutbox(next); err != nil {
		return err
	}
	s.outbox = next
	return nil
}

func (s *SupabaseUsageSync) enqueueBatch(batch supabaseUsageBatch) error {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	previous := s.outbox
	next := supabaseUsageOutbox{Batches: append(append([]supabaseUsageBatch(nil), previous.Batches...), batch)}
	for len(next.Batches) > s.maxOutboxBatches || s.outboxSize(next) > s.maxOutboxBytes {
		if len(next.Batches) <= 1 {
			s.droppedOutboxBatches.Add(1)
			return nil
		}
		next.Batches = next.Batches[1:]
		s.droppedOutboxBatches.Add(1)
	}
	if err := s.writeOutbox(next); err != nil {
		s.outbox = previous
		return err
	}
	s.outbox = next
	return nil
}

func (s *SupabaseUsageSync) outboxSize(state supabaseUsageOutbox) int64 {
	data, err := json.Marshal(state)
	if err != nil {
		return s.maxOutboxBytes + 1
	}
	return int64(len(data))
}

func (s *SupabaseUsageSync) writeOutbox(state supabaseUsageOutbox) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode outbox: %w", err)
	}
	if int64(len(data)) > s.maxOutboxBytes {
		return fmt.Errorf("outbox exceeds configured size limit")
	}
	if err := os.MkdirAll(filepath.Dir(s.outboxPath), 0o700); err != nil {
		return fmt.Errorf("create outbox directory: %w", err)
	}
	tmpPath := s.outboxPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	if err := os.Rename(tmpPath, s.outboxPath); err != nil {
		return fmt.Errorf("replace outbox: %w", err)
	}
	return nil
}

func buildSupabaseUsageDelta(record Record, authHashKey []byte) (usageDailyDelta, bool) {
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	provider := strings.TrimSpace(record.Provider)
	model := strings.TrimSpace(record.Model)
	if provider == "" || model == "" {
		return usageDailyDelta{}, false
	}

	detail, inputTokens, cachedTokens := canonicalTokenMetrics(record)
	totalTokens := detail.TotalTokens
	if totalTokens <= 0 && (inputTokens > 0 || detail.OutputTokens > 0) {
		totalTokens = inputTokens + detail.OutputTokens
	}
	status := record.Fail.StatusCode
	clientFault := availabilityClientFaultStatus(status)
	failed := record.Failed && !clientFault
	success := !record.Failed && !clientFault
	rateLimited := failed && (status == http.StatusTooManyRequests || status == http.StatusPaymentRequired)
	protocol := normalizeUsageProtocol(record.Protocol)
	authIdentity := strings.TrimSpace(record.AuthID)
	if authIdentity == "" {
		authIdentity = strings.TrimSpace(record.AuthIndex)
	}
	authHash := stableAuthHash(authHashKey, authIdentity)

	delta := usageDailyDelta{
		Day:           at.Format("2006-01-02"),
		Provider:      provider,
		Supplier:      strings.TrimSpace(record.Supplier),
		AuthHash:      authHash,
		Model:         model,
		Protocol:      protocol,
		OccurredAt:    at.Format(time.RFC3339Nano),
		SchemaVersion: SupabaseUsageSchemaVersion,
		Requests:      1,
		InputTokens:   inputTokens,
		OutputTokens:  detail.OutputTokens,
		TotalTokens:   totalTokens,
		CachedTokens:  cachedTokens,
	}
	if success {
		delta.Success = 1
	} else if failed {
		delta.Failed = 1
	} else if clientFault {
		delta.ExcludedClientFaults = 1
	}
	if rateLimited {
		delta.RateLimited = 1
	}
	if record.Latency > 0 {
		delta.LatencyMSSum = float64(record.Latency) / float64(time.Millisecond)
		delta.LatencyCount = 1
	}
	if record.TTFT > 0 {
		delta.TTFTMSSum = float64(record.TTFT) / float64(time.Millisecond)
		delta.TTFTCount = 1
	}
	if detail.OutputTokens > 0 && record.Latency > 0 {
		generation := record.Latency
		if record.TTFT > 0 && record.Latency > record.TTFT {
			generation = record.Latency - record.TTFT
		}
		if generation >= 100*time.Millisecond {
			delta.GenerationOutputTokens = detail.OutputTokens
			delta.GenerationSeconds = generation.Seconds()
		}
	}
	return delta, true
}

func mergeSupabaseUsageDelta(current, incoming usageDailyDelta) usageDailyDelta {
	if current.SchemaVersion == 0 {
		return incoming
	}
	current.Requests += incoming.Requests
	current.Success += incoming.Success
	current.Failed += incoming.Failed
	current.RateLimited += incoming.RateLimited
	current.ExcludedClientFaults += incoming.ExcludedClientFaults
	current.InputTokens += incoming.InputTokens
	current.OutputTokens += incoming.OutputTokens
	current.TotalTokens += incoming.TotalTokens
	current.CachedTokens += incoming.CachedTokens
	current.LatencyMSSum += incoming.LatencyMSSum
	current.LatencyCount += incoming.LatencyCount
	current.TTFTMSSum += incoming.TTFTMSSum
	current.TTFTCount += incoming.TTFTCount
	current.GenerationOutputTokens += incoming.GenerationOutputTokens
	current.GenerationSeconds += incoming.GenerationSeconds
	if incoming.OccurredAt > current.OccurredAt {
		current.OccurredAt = incoming.OccurredAt
	}
	return current
}

func canonicalTokenMetrics(record Record) (Detail, int64, int64) {
	detail := EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	inputTokens := detail.InputTokens
	cachedTokens := detail.CacheReadTokens
	if detail.TokenBreakdown.Valid() &&
		detail.TokenBreakdown.Quality != TokenAccountingQualityInconsistent &&
		detail.TokenBreakdown.Input.TotalTokens > 0 {
		inputTokens = detail.TokenBreakdown.Input.TotalTokens
		cachedTokens = detail.TokenBreakdown.Input.CacheReadTokens
		if cachedTokens == 0 && detail.CachedTokens > 0 &&
			detail.TokenBreakdown.Input.CacheWriteTokens == 0 && detail.CacheCreationTokens == 0 {
			cachedTokens = detail.CachedTokens
		}
	} else if cachedTokens == 0 {
		cachedTokens = detail.CachedTokens
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens && inputTokens > 0 {
		cachedTokens = inputTokens
	}
	return detail, inputTokens, cachedTokens
}

func availabilityClientFaultStatus(status int) bool {
	switch status {
	case 499, http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// isProviderCompatibilityFailure detects upstream 4xx responses that reflect
// provider capability/compatibility problems (model unavailable, protocol
// format rejection, unsupported features) rather than client request mistakes.
// Such failures must count against provider availability instead of being
// excluded as client faults.
func isProviderCompatibilityFailure(body string) bool {
	lower := strings.ToLower(strings.TrimSpace(body))
	if lower == "" {
		return false
	}
	markers := []string{
		"model is unavailable",
		"model unavailable",
		"model not found",
		"model_not_found",
		"must be in",
		"reasoning_content",
		"not supported",
		"unsupported",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func normalizeUsageProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "chat", "responses", "messages":
		return protocol
	default:
		return "unknown"
	}
}

func stableAuthHash(key []byte, identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write([]byte(identity))
	return hex.EncodeToString(digest.Sum(nil))
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
