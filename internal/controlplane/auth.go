// Package controlplane integrates CLIProxyAPI with the Orange control plane:
// it validates control-plane keys against the shared auth cache in Redis
// (same wire format as the gateway auth cache) and reports usage records into
// the odu.usage.recorded stream the metering worker consumes.
package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
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
}

// parseSnapshot mirrors apps/gateway/internal/auth.parseSnapshot.
func parseSnapshot(raw string) *Snapshot {
	if raw == "" {
		return nil
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
		}
	}
	return s
}

// KeySnapshot returns the control-plane snapshot injected by AuthMiddleware
// (nil when the request did not go through control-plane auth).
func KeySnapshot(ctx context.Context) *Snapshot {
	v, _ := ctx.Value(snapshotCtxKey{}).(*Snapshot)
	return v
}

type snapshotCtxKey struct{}

// WithSnapshot returns a context carrying the control-plane snapshot.
func WithSnapshot(ctx context.Context, snap *Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, snap)
}

// FromSnapshot reads the snapshot from a context (nil when absent).
func FromSnapshot(ctx context.Context) *Snapshot { return KeySnapshot(ctx) }

// AuthMiddleware validates Bearer keys against Redis auth:<sha256>. It only
// guards /v1/* paths that carry a Bearer header (management/UI routes and
// keyless local traffic keep their existing behavior).
func AuthMiddleware(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			c.Next()
			return
		}
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.Next()
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if raw == "" {
			c.Next()
			return
		}
		sum := sha256.Sum256([]byte(raw))
		kh := hex.EncodeToString(sum[:])
		val, err := rdb.Get(c.Request.Context(), "auth:"+kh).Result()
		if err != nil {
			// Cache miss: the key is not a control-plane key — let CPA's own
			// static api-keys (config) validate it as before (no regression
			// for pre-existing projects).
			c.Next()
			return
		}
		snap := parseSnapshot(val)
		if snap == nil {
			c.Next()
			return
		}
		if !snap.Valid {
			code := "key_revoked"
			if snap != nil && snap.Code != "" {
				code = snap.Code
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid api key (" + code + ")", "type": "auth_error", "code": code}})
			return
		}
		if snap.ExpireAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, snap.ExpireAt); err == nil && time.Now().After(t) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "api key expired", "type": "auth_error", "code": "key_expired"}})
				return
			}
		}
		// Per-key rate counters (60s windows; hard limits from the snapshot).
		if snap.RPM > 0 {
			allowed := rateIncr(rdb, c.Request.Context(), "cp:rpm:"+kh, 60)
			if allowed > snap.RPM {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "rpm limit exceeded", "type": "rate_limit_error", "code": "rpm_limit"}})
				return
			}
		}
		if snap.TPM > 0 {
			allowed := rateIncr(rdb, c.Request.Context(), "cp:tpm:"+kh, 60)
			if allowed > snap.TPM {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "tpm limit exceeded", "type": "rate_limit_error", "code": "tpm_limit"}})
				return
			}
		}
		ctx := context.WithValue(c.Request.Context(), snapshotCtxKey{}, snap)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
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
