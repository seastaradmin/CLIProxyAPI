package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/credentialweight"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

var (
	callbackForwardersMu  sync.Mutex
	callbackForwarders    = make(map[int]*callbackForwarder)
	authFileEntryMu       sync.Mutex
	errAuthFileMustBeJSON = errors.New("auth file must be .json")
	errAuthFileNotFound   = errors.New("auth file not found")
	errPluginVirtualAuth  = errors.New("plugin virtual auth cannot be modified directly; edit or delete the source auth file")
	newCodexOAuthService  = func(cfg *config.Config) codexOAuthService { return codex.NewCodexAuth(cfg) }
)

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c)
		return
	}
	nameFilter := strings.TrimSpace(c.Query("name"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	auths := h.authManager.List()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if !matchesAuthFileLookup(auth, nameFilter, authIndexFilter) {
			continue
		}
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(200, gin.H{"files": files})
}

func lockedAuthIndex(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	authFileEntryMu.Lock()
	defer authFileEntryMu.Unlock()
	return strings.TrimSpace(auth.EnsureIndex())
}

func matchesAuthFileLookup(auth *coreauth.Auth, name string, authIndex string) bool {
	if auth == nil {
		return false
	}
	if name != "" && strings.TrimSpace(auth.ID) != name && strings.TrimSpace(auth.FileName) != name {
		return false
	}
	if authIndex != "" && lockedAuthIndex(auth) != authIndex {
		return false
	}
	return true
}

func (h *Handler) lookupAuthFile(name string, authIndex string) (*coreauth.Auth, bool) {
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if h == nil || h.authManager == nil || name == "" {
		return nil, false
	}
	if authIndex == "" {
		if auth, ok := h.authManager.GetByID(name); ok {
			return auth, true
		}
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth != nil && strings.TrimSpace(auth.FileName) == name {
				return auth, true
			}
		}
		return nil, false
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if matchesAuthFileLookup(auth, name, authIndex) {
			return auth, true
		}
	}
	return nil, false
}

// GetAuthFileModels returns the models supported by a specific auth file
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	// Try to find auth ID via authManager
	var authID string
	if h.authManager != nil {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name || auth.ID == name {
				authID = auth.ID
				break
			}
		}
	}

	if authID == "" {
		authID = name // fallback to filename as ID
	}

	// Get models from registry
	reg := registry.GetGlobalRegistry()
	models := reg.GetModelsForClient(authID)

	result := make([]gin.H, 0, len(models))
	for _, m := range models {
		entry := gin.H{
			"id": m.ID,
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Type != "" {
			entry["type"] = m.Type
		}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		result = append(result, entry)
	}

	c.JSON(200, gin.H{"models": result})
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context) {
	nameFilter := strings.TrimSpace(c.Query("name"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	files := make([]gin.H, 0)
	if authIndexFilter != "" {
		c.JSON(200, gin.H{"files": files})
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if nameFilter != "" && name != nameFilter {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field
			full := filepath.Join(h.cfg.AuthDir, name)
			if data, errRead := os.ReadFile(full); errRead == nil {
				typeValue := gjson.GetBytes(data, "type").String()
				emailValue := gjson.GetBytes(data, "email").String()
				fileData["type"] = typeValue
				fileData["email"] = emailValue
				if projectID := strings.TrimSpace(gjson.GetBytes(data, "project_id").String()); projectID != "" {
					fileData["project_id"] = projectID
				}
				if pv := gjson.GetBytes(data, "priority"); pv.Exists() {
					switch pv.Type {
					case gjson.Number:
						fileData["priority"] = int(pv.Int())
					case gjson.String:
						if parsed, errAtoi := strconv.Atoi(strings.TrimSpace(pv.String())); errAtoi == nil {
							fileData["priority"] = parsed
						}
					}
				}
				if wv := gjson.GetBytes(data, coreauth.AttributeWeight); wv.Exists() {
					var rawWeight string
					switch wv.Type {
					case gjson.Number:
						rawWeight = wv.Raw
					case gjson.String:
						rawWeight = wv.String()
					}
					if rawWeight != "" {
						if weight, errWeight := credentialweight.ParseString(rawWeight); errWeight == nil {
							fileData[coreauth.AttributeWeight] = weight
						}
					}
				}
				if nv := gjson.GetBytes(data, "note"); nv.Exists() && nv.Type == gjson.String {
					if trimmed := strings.TrimSpace(nv.String()); trimmed != "" {
						fileData["note"] = trimmed
					}
				}
				if wv := gjson.GetBytes(data, "websockets"); wv.Exists() {
					switch wv.Type {
					case gjson.True:
						fileData["websockets"] = true
					case gjson.False:
						fileData["websockets"] = false
					case gjson.String:
						if parsed, errParse := strconv.ParseBool(strings.TrimSpace(wv.String())); errParse == nil {
							fileData["websockets"] = parsed
						}
					}
				}
				if requestRetry, okRetry := authFileRequestRetryFromJSON(data); okRetry {
					fileData["request_retry"] = requestRetry
				}
			}

			files = append(files, fileData)
		}
	}
	c.JSON(200, gin.H{"files": files})
}

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth) gin.H {
	authFileEntryMu.Lock()
	defer authFileEntryMu.Unlock()
	return h.buildAuthFileEntryLocked(auth)
}

type authCooldownSummary struct {
	inCooldown bool
	until      time.Time
	kind       string
	reason     string
	models     []gin.H
}

func authCooldownKind(quota coreauth.QuotaState, statusMessage string, lastErr *coreauth.Error) string {
	if kind := strings.TrimSpace(quota.Kind); kind != "" {
		switch strings.ToLower(kind) {
		case "quota":
			return string(coreauth.UsageErrorKindQuotaExhausted)
		case "rate_limit", "rate-limited":
			return string(coreauth.UsageErrorKindRateLimited)
		}
		return kind
	}
	if kind := coreauth.ClassifyUsageError(lastErrorStatusCode(lastErr), lastErrorCode(lastErr), lastErrorMessage(lastErr)); kind != "" {
		return string(kind)
	}
	if strings.EqualFold(strings.TrimSpace(quota.Reason), "rate_limit") || strings.Contains(strings.ToLower(statusMessage), "rate limit") {
		return string(coreauth.UsageErrorKindRateLimited)
	}
	if strings.EqualFold(strings.TrimSpace(quota.Reason), "quota") || strings.Contains(strings.ToLower(statusMessage), "quota") {
		return string(coreauth.UsageErrorKindQuotaExhausted)
	}
	return "cooldown"
}

func lastErrorStatusCode(err *coreauth.Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func lastErrorCode(err *coreauth.Error) string {
	if err == nil {
		return ""
	}
	return err.Code
}

func lastErrorMessage(err *coreauth.Error) string {
	if err == nil {
		return ""
	}
	return err.Message
}

func cooldownDisplayReason(kind, statusMessage string, quota coreauth.QuotaState) string {
	switch kind {
	case string(coreauth.UsageErrorKindQuotaExhausted):
		return "quota exhausted"
	case string(coreauth.UsageErrorKindRateLimited):
		return "rate limited"
	}
	if message := strings.TrimSpace(statusMessage); message != "" {
		return message
	}
	return strings.TrimSpace(quota.Reason)
}

func addAuthCooldownDetail(summary *authCooldownSummary, now time.Time, model string, unavailable bool, next time.Time, quota coreauth.QuotaState, statusMessage string, lastErr *coreauth.Error) {
	if summary == nil || !unavailable || next.IsZero() {
		return
	}
	if quota.NextRecoverAt.After(next) {
		next = quota.NextRecoverAt
	}
	if !next.After(now) {
		return
	}
	kind := authCooldownKind(quota, statusMessage, lastErr)
	reason := cooldownDisplayReason(kind, statusMessage, quota)
	if summary.until.IsZero() || next.Before(summary.until) {
		summary.until = next
		summary.kind = kind
		summary.reason = reason
	}
	summary.inCooldown = true
	if model == "" {
		return
	}
	summary.models = append(summary.models, gin.H{
		"model":          model,
		"kind":           kind,
		"reason":         reason,
		"status":         "cooling",
		"cooldown_until": next,
	})
}

func authCooldownSummaryFor(auth *coreauth.Auth, now time.Time) authCooldownSummary {
	var summary authCooldownSummary
	if auth == nil {
		return summary
	}
	if auth.Unavailable {
		addAuthCooldownDetail(&summary, now, "", true, auth.NextRetryAfter, auth.Quota, auth.StatusMessage, auth.LastError)
	}
	models := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := auth.ModelStates[model]
		if state == nil {
			continue
		}
		addAuthCooldownDetail(&summary, now, model, state.Unavailable, state.NextRetryAfter, state.Quota, state.StatusMessage, state.LastError)
	}
	return summary
}

func (h *Handler) buildAuthFileEntryLocked(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	auth.EnsureIndex()
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	now := time.Now()
	cooldown := authCooldownSummaryFor(auth, now)
	entry := gin.H{
		"id":             auth.ID,
		"auth_index":     auth.Index,
		"name":           name,
		"type":           strings.TrimSpace(auth.Provider),
		"provider":       strings.TrimSpace(auth.Provider),
		"label":          auth.Label,
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"problem_kind":   "none",
		"in_cooldown":    cooldown.inCooldown,
		"runtime_only":   runtimeOnly,
		"source":         "memory",
		"size":           int64(0),
	}
	entry["success"] = auth.Success
	entry["failed"] = auth.Failed
	entry["recent_requests"] = auth.RecentRequestsSnapshot(now)
	if auth.Quota.Exceeded {
		entry["quota"] = auth.Quota
	}
	entry["cooling"] = cooldown.inCooldown
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		entry["problem_kind"] = "disabled"
	} else if cooldown.inCooldown {
		entry["problem_kind"] = "cooldown"
	} else if auth.Status == coreauth.StatusError || auth.StatusMessage != "" {
		entry["problem_kind"] = "error"
	}
	if cooldown.inCooldown {
		entry["cooldown_kind"] = cooldown.kind
		entry["cooldown_reason"] = cooldown.reason
		entry["cooldown_until"] = cooldown.until
		if len(cooldown.models) > 0 {
			entry["cooldown_models"] = cooldown.models
		}
	}
	if auth.Metadata != nil {
		if healthCheck, ok := auth.Metadata[healthCheckMetadataKey]; ok {
			entry["health_check"] = healthCheck
		}
		if excludedModels := authFileExcludedModels(auth.Metadata); len(excludedModels) > 0 {
			entry["excluded_models"] = excludedModels
		}
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if projectID := authProjectID(auth); projectID != "" {
		entry["project_id"] = projectID
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if info, err := os.Stat(path); err == nil {
			entry["size"] = info.Size()
			entry["modtime"] = info.ModTime()
		} else if os.IsNotExist(err) {
			// Hide credentials removed from disk but still lingering in memory.
			if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
				return nil
			}
			entry["source"] = "memory"
		} else {
			log.WithError(err).Warnf("failed to stat auth file %s", path)
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	// Expose priority from Attributes (set by synthesizer from JSON "priority" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if p := strings.TrimSpace(authAttribute(auth, "priority")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			entry["priority"] = parsed
		}
	} else if auth.Metadata != nil {
		if rawPriority, ok := auth.Metadata["priority"]; ok {
			switch v := rawPriority.(type) {
			case float64:
				entry["priority"] = int(v)
			case int:
				entry["priority"] = v
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					entry["priority"] = parsed
				}
			}
		}
	}
	// Expose note from Attributes (set by synthesizer from JSON "note" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		entry["note"] = note
	} else if auth.Metadata != nil {
		if rawNote, ok := auth.Metadata["note"].(string); ok {
			if trimmed := strings.TrimSpace(rawNote); trimmed != "" {
				entry["note"] = trimmed
			}
		}
	}
	if weight, ok := authWeightValue(auth); ok {
		entry[coreauth.AttributeWeight] = weight
	}
	if websockets, ok := authWebsocketsValue(auth); ok {
		entry["websockets"] = websockets
	}
	if requestRetry, ok := auth.RequestRetryOverride(); ok {
		entry["request_retry"] = requestRetry
	}
	return entry
}

func authFileRequestRetryFromJSON(data []byte) (int, bool) {
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return 0, false
	}
	return (&coreauth.Auth{Metadata: metadata}).RequestRetryOverride()
}

func authWeightValue(auth *coreauth.Auth) (int64, bool) {
	if auth == nil {
		return 0, false
	}
	if rawWeight := strings.TrimSpace(authAttribute(auth, coreauth.AttributeWeight)); rawWeight != "" {
		weight, errWeight := credentialweight.ParseString(rawWeight)
		return weight, errWeight == nil
	}
	if auth.Metadata == nil {
		return 0, false
	}
	rawWeight, ok := auth.Metadata[coreauth.AttributeWeight]
	if !ok || rawWeight == nil {
		return 0, false
	}
	weight, errWeight := credentialweight.ParseValue(rawWeight)
	return weight, errWeight == nil
}

func authWebsocketsValue(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false, false
	}
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

func authProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["project_id"].(string); ok {
			if projectID := strings.TrimSpace(v); projectID != "" {
				return projectID
			}
		}
	}
	if auth.Attributes != nil {
		if projectID := strings.TrimSpace(auth.Attributes["project_id"]); projectID != "" {
			return projectID
		}
	}
	return ""
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return nil
	}

	result := gin.H{}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
		result["plan_type"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
		result["chatgpt_subscription_active_until"] = v
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func isUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}
