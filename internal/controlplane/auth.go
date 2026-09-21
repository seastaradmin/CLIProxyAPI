// Package controlplane integrates CLIProxyAPI with the Orange control plane:
// it validates control-plane keys against the shared auth cache in Redis
// (same wire format as the gateway auth cache) and reports usage records into
// the odu.usage.recorded stream the metering worker consumes.
package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Snapshot is the control-plane auth cache entry (wire format v=1;valid=…;wl=…).
type Snapshot struct {
	Valid          bool
	KeyID          string
	ProjectID      string
	TenantID       string
	ConsumerID     string
	Code           string
	ModelWhitelist []string
	ExpireAt       string
	RPM            int64
	TPM            int64
	// MaxBudget is the key's monthly CNY spend limit (0 = unlimited), enforced
	// against the cp:budget:key:<id>:<YYYY-MM> counter that metering settles.
	MaxBudget float64
	// IPWhitelistRule / IPBlacklistRule carry the synthesized access-control
	// rules (enterprise > team > individual > key). The whitelist is layered:
	// layers are split by "|" and entries within a layer by ","; an empty layer
	// means unrestricted, and every non-empty layer must match. The blacklist is
	// a union where any hit denies. Empty means no IP restriction.
	IPWhitelistRule string
	IPBlacklistRule string
}

// parseSnapshot mirrors apps/gateway/internal/auth.parseSnapshot.
func parseSnapshot(raw string) *Snapshot {
	if raw == "" {
		return nil
	}
	// Tolerate legacy entries JSON-quoted by older control-api caches: strip
	// one wrapping quote pair so the last k=v field parses cleanly.
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	s := &Snapshot{}
	for _, f := range strings.Split(raw, ";") {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "valid":
			s.Valid = kv[1] == "true"
		case "key":
			s.KeyID = kv[1]
		case "proj":
			s.ProjectID = kv[1]
		case "tenant":
			s.TenantID = kv[1]
		case "consumer":
			s.ConsumerID = kv[1]
		case "code":
			s.Code = kv[1]
		case "wl":
			s.ModelWhitelist = strings.Split(kv[1], ",")
			if len(s.ModelWhitelist) == 1 && s.ModelWhitelist[0] == "" {
				s.ModelWhitelist = nil
			}
		case "exp":
			s.ExpireAt = kv[1]
		case "mb":
			if v, err := strconv.ParseFloat(kv[1], 64); err == nil {
				s.MaxBudget = v
			}
		case "ipw":
			s.IPWhitelistRule = kv[1]
		case "ipb":
			s.IPBlacklistRule = kv[1]
		}
	}
	return s
}

// formatCNY renders an amount rounded to 6 decimals without trailing zeros
// (also hides float32 storage noise from PG REAL columns: 0.0020000000… → 0.002).
func formatCNY(v float64) string {
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// KeySnapshot returns the control-plane snapshot injected by AuthMiddleware
// (nil when the request did not go through control-plane auth).
func KeySnapshot(ctx context.Context) *Snapshot {
	v, _ := ctx.Value(snapshotCtxKey{}).(*Snapshot)
	return v
}

// requestBodyModel reads the top-level "model" field of the request and
// restores the body so downstream handlers can still consume it (streaming
// requests included).
func requestBodyModel(c *gin.Context) string {
	if c.Request.Body == nil {
		return ""
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return strings.TrimSpace(req.Model)
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// clientIP extracts the caller address: the first hop of X-Forwarded-For wins
// (behind a proxy such as nginx this is the real client IP), falling back to
// RemoteAddr with the port stripped.
func clientIP(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if xff := strings.TrimSpace(c.Request.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	host := c.Request.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	// IPv6 literals may arrive bracketed ([::1]); strip before matching.
	host = strings.Trim(host, "[]")
	return strings.TrimSpace(host)
}

// ipEntryMatches reports whether ip is covered by an entry (exact IP or CIDR).
func ipEntryMatches(entry, ipStr string) bool {
	entry = strings.TrimSpace(entry)
	ipStr = strings.TrimSpace(ipStr)
	if entry == "" || ipStr == "" {
		return false
	}
	if _, ipnet, err := net.ParseCIDR(entry); err == nil {
		if ip := net.ParseIP(ipStr); ip != nil {
			return ipnet.Contains(ip)
		}
		// An exact-IP entry may also be written in CIDR form ("1.2.3.4/32"),
		// which ParseCIDR above already handled.
		return false
	}
	return entry == ipStr
}

// ipDenied enforces the access-control rules carried in the snapshot:
//   - whitelist ipw= is layered ("|"-separated): every non-empty layer must match.
//   - blacklist ipb= is a union: any hit denies.
//
// All rules empty means no IP restriction. Returns (denied, reason).
func ipDenied(snap *Snapshot, ipStr string) (bool, string) {
	if snap == nil {
		return false, ""
	}
	// Blacklist first (union, takes priority).
	if snap.IPBlacklistRule != "" {
		for _, entry := range strings.Split(snap.IPBlacklistRule, ",") {
			if ipEntryMatches(entry, ipStr) {
				return true, "ip_blacklisted"
			}
		}
	}
	// Whitelist (intersection: every non-empty layer must match).
	if snap.IPWhitelistRule != "" {
		for _, layer := range strings.Split(snap.IPWhitelistRule, "|") {
			if layer == "" {
				continue
			}
			hit := false
			for _, entry := range strings.Split(layer, ",") {
				if ipEntryMatches(entry, ipStr) {
					hit = true
					break
				}
			}
			if !hit {
				return true, "ip_not_allowed"
			}
		}
	}
	return false, ""
}

type snapshotCtxKey struct{}

type keyHashCtxKey struct{}

const tpmUsageKeyPrefix = "cp:tpm:usage:"

// WithSnapshot returns a context carrying the control-plane snapshot.
func WithSnapshot(ctx context.Context, snap *Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, snap)
}

// FromSnapshot reads the snapshot from a context (nil when absent).
func FromSnapshot(ctx context.Context) *Snapshot { return KeySnapshot(ctx) }

func keyHashFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(keyHashCtxKey{}).(string)
	return value
}

func requestAuthKeys(r *http.Request) []string {
	if r == nil {
		return nil
	}
	keys := make([]string, 0, 3)
	appendKey := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}

	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			appendKey(parts[1])
		}
	}
	appendKey(r.Header.Get("X-Api-Key"))
	appendKey(r.Header.Get("X-Goog-Api-Key"))
	return keys
}

func keyHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func tpmUsageKey(keyHash string) string {
	if strings.TrimSpace(keyHash) == "" {
		return ""
	}
	return tpmUsageKeyPrefix + keyHash
}

// AuthMiddleware validates control-plane keys against Redis auth:<sha256>. It
// accepts the native consumer-key headers used by OpenAI-compatible,
// Anthropic, and Google clients.
//
// Read-through caching: on a Redis hit it refreshes the key's TTL (touch-on-use),
// so actively-used keys never expire; on a miss it asks the control-plane to
// validate the key (which publishes the snapshot to Redis), then re-reads it.
// No periodic full refresh is needed.
func AuthMiddleware(rdb *redis.Client, controlAPIURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil || !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			c.Next()
			return
		}
		keys := requestAuthKeys(c.Request)
		if len(keys) == 0 {
			c.Next()
			return
		}

		var invalidCode string
		var expired bool
		for _, raw := range keys {
			kh := keyHash(raw)
			snap, found := loadSnapshot(c.Request.Context(), rdb, controlAPIURL, raw, kh)
			if !found || snap == nil {
				continue
			}
			if !snap.Valid {
				// KEY_NOT_FOUND = this bearer is not a control-plane key.
				// Fail open so CPA's own api-keys (config.yaml) can still authenticate.
				// Only known-dead control-plane keys (revoked / disabled) 401 here.
				code := defaultString(snap.Code, "key_revoked")
				if unknownControlPlaneKey(code) {
					continue
				}
				invalidCode = code
				continue
			}
			if snap.ExpireAt != "" {
				if t, err := time.Parse(time.RFC3339Nano, snap.ExpireAt); err == nil && time.Now().After(t) {
					expired = true
					continue
				}
			}
			// IP access control runs first: rejecting dirty traffic and scanners
			// by IP is the cheapest check, so it saves the model/budget/RPM work
			// below. Whitelist layers intersect, blacklist entries union.
			if denied, reason := ipDenied(snap, clientIP(c)); denied {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{
					"message": "client ip not allowed (" + reason + ")",
					"type":    "invalid_request_error",
					"code":    "ip_not_allowed",
				}})
				return
			}
			// Model whitelist: wl= is the key's effective model set, bounded by
			// the control plane's active catalog (delisted/archived models are no
			// longer published into any key's wl=). An empty set allows everything
			// (legacy keys); a non-empty set must contain this request's model,
			// otherwise the request is rejected without spending budget/RPM checks.
			if len(snap.ModelWhitelist) > 0 {
				if model := requestBodyModel(c); model != "" && !containsStr(snap.ModelWhitelist, model) {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{
						"message": "model not allowed for this key",
						"type":    "invalid_request_error",
						"code":    "model_not_allowed",
					}})
					return
				}
			}
			// Monthly CNY budget: block before dispatch once the key's settled
			// spend (counter fed by metering) reached its max_budget. Checked
			// before RPM so exhausted keys don't burn rate-limit slots.
			if snap.MaxBudget > 0 {
				if used, ok := monthlySpend(rdb, c.Request.Context(), snap.KeyID); ok && used+1e-9 >= snap.MaxBudget {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
						"message": fmt.Sprintf("monthly budget exceeded for this key (used %s of %s CNY)",
							formatCNY(used), formatCNY(snap.MaxBudget)),
						"type": "insufficient_quota",
						"code": "insufficient_quota",
					}})
					return
				}
			}
			if snap.TPM > 0 && !withinTPM(rdb, c.Request.Context(), kh, snap.TPM) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "tpm limit exceeded", "type": "rate_limit_error", "code": "tpm_limit"}})
				return
			}
			// Touch-on-use: keep the auth cache alive while the key is actively used
			// (read-through caching; idle keys expire naturally).
			authCacheKey := "auth:" + kh
			_ = rdb.Expire(c.Request.Context(), authCacheKey, 300*time.Second).Err()
			// RPM remains request-based and is enforced before dispatch.
			if snap.RPM > 0 {
				allowed := rateIncr(rdb, c.Request.Context(), "cp:rpm:"+kh, 60)
				if allowed > snap.RPM {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "rpm limit exceeded", "type": "rate_limit_error", "code": "rpm_limit"}})
					return
				}
			}
			ctx := context.WithValue(c.Request.Context(), snapshotCtxKey{}, snap)
			ctx = context.WithValue(ctx, keyHashCtxKey{}, kh)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}
		if invalidCode != "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid api key (" + invalidCode + ")", "type": "auth_error", "code": invalidCode}})
			return
		}
		if expired {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "api key expired", "type": "auth_error", "code": "key_expired"}})
			return
		}
		// Not a control-plane key. Let CPA's own access providers validate it.
		c.Next()
	}
}

func loadSnapshot(ctx context.Context, rdb *redis.Client, controlAPIURL, raw, kh string) (*Snapshot, bool) {
	if rdb == nil || strings.TrimSpace(raw) == "" || strings.TrimSpace(kh) == "" {
		return nil, false
	}
	authCacheKey := "auth:" + kh
	val, err := rdb.Get(ctx, authCacheKey).Result()
	if err != nil {
		// Cache miss or Redis unavailable: read-through to the control-plane's
		// validate endpoint, which answers from SQL and keeps serving. Its response
		// carries the complete effective snapshot, so enforcement does not
		// depend on Redis being back. When Redis is reachable the response is
		// also written back so the next request hits the cache (unchanged behavior).
		if controlAPIURL == "" {
			return nil, false
		}
		resp, ok := callControlValidateSnapshot(ctx, controlAPIURL, raw)
		if !ok || resp == nil {
			return nil, false
		}
		if !resp.Valid {
			return &Snapshot{Valid: false, KeyID: resp.KeyID, Code: resp.Code}, true
		}
		snap := &Snapshot{
			Valid: true, KeyID: resp.KeyID, ProjectID: resp.ProjectID, TenantID: resp.TenantID,
			ModelWhitelist: resp.ModelWhitelist, ExpireAt: resp.ExpireAt,
			RPM: resp.RPM, TPM: resp.TPM, MaxBudget: resp.MaxBudget,
			IPWhitelistRule: resp.IPWhitelistRule, IPBlacklistRule: resp.IPBlacklistRule,
		}
		// Best-effort write-back: the next request goes through the fast path
		// once Redis recovers; a failure here is fine (we already have the
		// snapshot in hand).
		_ = rdb.Set(ctx, authCacheKey, encodeSnapshotForCache(snap), 300*time.Second).Err()
		return snap, true
	}
	snap := parseSnapshot(val)
	return snap, snap != nil
}

func withinTPM(rdb *redis.Client, ctx context.Context, kh string, limit int64) bool {
	if rdb == nil || limit <= 0 {
		return true
	}
	used, err := rdb.Get(ctx, tpmUsageKey(kh)).Int64()
	if err != nil && err != redis.Nil {
		// Preserve the existing fail-open behavior when Redis is unavailable.
		return true
	}
	return used < limit
}

// budgetKeyPrefix / budgetMonthKey mirror the metering-side settler: the key's
// settled monthly spend lives at cp:budget:key:<keyID>:<YYYY-MM> (UTC month).
const budgetKeyPrefix = "cp:budget:key:"

func budgetMonthKey(keyID string, at time.Time) string {
	return budgetKeyPrefix + keyID + ":" + at.UTC().Format("2006-01")
}

// monthlySpend reads the key's settled month-to-date spend. ok=false means the
// counter is unavailable (missing or Redis error) — callers fail open, same as
// the TPM rail.
func monthlySpend(rdb *redis.Client, ctx context.Context, keyID string) (float64, bool) {
	if rdb == nil || strings.TrimSpace(keyID) == "" {
		return 0, false
	}
	used, err := rdb.Get(ctx, budgetMonthKey(keyID, time.Now())).Float64()
	if err != nil && err != redis.Nil {
		return 0, false
	}
	return used, true
}

// callControlValidateSnapshot asks the control-plane (/internal/v1/keys/validate)
// to validate a key from SQL (Redis-independent read-through fallback). The
// response carries the full effective snapshot (model set + limits), so the
// caller can enforce policy without waiting for Redis.
func callControlValidateSnapshot(ctx context.Context, controlAPIURL, key string) (*validateSnapshot, bool) {
	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(controlAPIURL, "/")+"/internal/v1/keys/validate", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out validateSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false
	}
	return &out, true
}

// validateSnapshot mirrors the shared /internal/v1/keys/validate response DTO.
type validateSnapshot struct {
	Valid           bool     `json:"valid"`
	KeyID           string   `json:"key_id,omitempty"`
	ProjectID       string   `json:"project_id,omitempty"`
	TenantID        string   `json:"tenant_id,omitempty"`
	ModelWhitelist  []string `json:"model_whitelist,omitempty"`
	Code            string   `json:"code,omitempty"`
	ExpireAt        string   `json:"expire_at,omitempty"`
	RPM             int64    `json:"rpm,omitempty"`
	TPM             int64    `json:"tpm,omitempty"`
	MaxBudget       float64  `json:"max_budget,omitempty"`
	IPWhitelistRule string   `json:"ip_whitelist_rule,omitempty"`
	IPBlacklistRule string   `json:"ip_blacklist_rule,omitempty"`
}

// encodeSnapshotForCache serializes a snapshot in the auth cache wire format
// (same as the control-plane PublishAuth), used for the best-effort write-back
// after a read-through validation.
func encodeSnapshotForCache(s *Snapshot) string {
	if s == nil {
		return ""
	}
	valid := s.Valid
	if !valid {
		valid = s.Code == "" || s.Code == "key_not_found"
	}
	return fmt.Sprintf("v=1;valid=%t;key=%s;proj=%s;tenant=%s;consumer=%s;code=%s;wl=%s;exp=%s;rpm=%d;tpm=%d;mb=%s;ipw=%s;ipb=%s",
		valid, s.KeyID, s.ProjectID, s.TenantID, s.ConsumerID, s.Code,
		strings.Join(s.ModelWhitelist, ","), s.ExpireAt, s.RPM, s.TPM,
		strconv.FormatFloat(s.MaxBudget, 'f', -1, 64), s.IPWhitelistRule, s.IPBlacklistRule)
}

// rateIncr increments a counter and refreshes its TTL, returning the new value.
func rateIncr(rdb *redis.Client, ctx context.Context, key string, windowSec int) int64 {
	n, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0
	}
	if n == 1 {
		_ = rdb.Expire(ctx, key, time.Duration(windowSec)*time.Second).Err()
	}
	return n
}

// NewRedis connects to the shared cache (CP_REDIS_ADDR, default localhost:6379).
// Returns nil when the address is explicitly empty (integration disabled).
func NewRedis() *redis.Client {
	addr := envOr("CP_REDIS_ADDR", "localhost:6379")
	if addr == "" {
		return nil
	}
	return redis.NewClient(&redis.Options{Addr: addr})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// unknownControlPlaneKey reports whether validate/SQL said this bearer is not
// a control-plane key at all. CPA static api-keys take this path and must
// fall through to config.yaml providers instead of 401.
func unknownControlPlaneKey(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "", "KEY_NOT_FOUND", "KEYNOTFOUND":
		return true
	default:
		return false
	}
}
