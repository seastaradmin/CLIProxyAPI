package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	apiHandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	healthCheckMetadataKey = "management_health_check"
	healthCheckTimeout     = 75 * time.Second
)

type authFileHealthCheckRequest struct {
	Name                          string   `json:"name"`
	AuthIndex                     string   `json:"auth_index"`
	Model                         string   `json:"model"`
	Protocols                     []string `json:"protocols"`
	QuarantineOnCredentialFailure *bool    `json:"quarantine_on_credential_failure"`
	RecordResult                  *bool    `json:"record_result"`
	RecordProbeResult             *bool    `json:"record_probe_result"`
}

type authFileHealthProtocolResult struct {
	Protocol   string `json:"protocol"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Reply      string `json:"reply,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"error_type,omitempty"`
}

type authFileHealthCheckResponse struct {
	Status      string                         `json:"status"`
	Model       string                         `json:"model"`
	CheckedAt   time.Time                      `json:"checked_at"`
	Quarantined bool                           `json:"quarantined"`
	Results     []authFileHealthProtocolResult `json:"results"`
}

type healthProtocolSpec struct {
	name          string
	handlerFormat string
	payload       func(string) []byte
}

var healthProtocolSpecs = map[string]healthProtocolSpec{
	"responses": {
		name:          "responses",
		handlerFormat: OpenaiResponse,
		payload: func(model string) []byte {
			return mustMarshalHealthPayload(map[string]any{
				"model": model,
				"input": "Reply with exactly: OK",
			})
		},
	},
	"chat": {
		name:          "chat",
		handlerFormat: OpenAI,
		payload: func(model string) []byte {
			return mustMarshalHealthPayload(map[string]any{
				"model": model,
				"messages": []map[string]string{{
					"role":    "user",
					"content": "Reply with exactly: OK",
				}},
			})
		},
	},
	"messages": {
		name:          "messages",
		handlerFormat: Claude,
		payload: func(model string) []byte {
			return mustMarshalHealthPayload(map[string]any{
				"model":      model,
				"max_tokens": 32,
				"messages": []map[string]string{{
					"role":    "user",
					"content": "Reply with exactly: OK",
				}},
			})
		},
	},
}

// HealthCheckAuthFile runs a real, non-streaming model conversation against a
// specific credential. The credential is pinned so scheduler failover cannot
// hide a broken account.
func (h *Handler) HealthCheckAuthFile(c *gin.Context) {
	if h == nil || h.authManager == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req authFileHealthCheckRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	auth := h.resolveHealthCheckAuth(req.Name, req.AuthIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file is disabled; enable it before health check"})
		return
	}

	specs, errProtocols := normalizeHealthProtocols(req.Protocols)
	if errProtocols != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errProtocols.Error()})
		return
	}

	// The probe pins this credential, so the model must be one of its own
	// routing names. Failing early avoids a misleading router error that
	// lists unrelated providers serving the same name.
	if reg := registry.GetGlobalRegistry(); reg != nil {
		if registered := reg.GetModelsForClient(auth.ID); len(registered) > 0 {
			supported := false
			routes := make([]string, 0, len(registered))
			for _, m := range registered {
				if m == nil {
					continue
				}
				id := strings.TrimSpace(m.ID)
				if id == "" {
					continue
				}
				routes = append(routes, id)
				if strings.EqualFold(id, model) {
					supported = true
				}
			}
			if !supported {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf(
						"credential does not serve model %q; its routing names are: %s (aliased models hide the upstream name unless fork is enabled)",
						model, strings.Join(routes, ", "),
					),
				})
				return
			}
		}
	}

	baseHandler := apiHandlers.NewBaseAPIHandlers(&h.cfg.SDKConfig, h.authManager)
	results := make([]authFileHealthProtocolResult, len(specs))
	requestContext := c.Request.Context()
	authID := auth.ID
	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = runAuthFileHealthProtocol(
				requestContext,
				baseHandler,
				authID,
				model,
				specs[index],
			)
		}(i)
	}
	wg.Wait()
	if latestAuth, ok := h.authManager.GetByID(authID); ok {
		auth = latestAuth
	}

	checkedAt := time.Now().UTC()
	status := summarizeHealthCheckStatus(results)
	quarantineOnCredentialFailure := true
	if req.QuarantineOnCredentialFailure != nil {
		quarantineOnCredentialFailure = *req.QuarantineOnCredentialFailure
	}
	quarantined := quarantineOnCredentialFailure && status == "error" && hasCredentialHealthFailure(results)
	recordResult := true
	if req.RecordResult != nil {
		recordResult = *req.RecordResult
	}
	recordProbeResult := req.RecordProbeResult != nil && *req.RecordProbeResult
	if recordProbeResult {
		if errPersist := h.persistModelProbeResults(c.Request.Context(), auth, model, checkedAt, results); errPersist != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist model probe result: %v", errPersist)})
			return
		}
	}
	if recordResult {
		if errPersist := h.persistAuthFileHealthCheck(c.Request.Context(), auth, model, status, checkedAt, results, quarantined); errPersist != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist health check: %v", errPersist)})
			return
		}
	}

	c.JSON(http.StatusOK, authFileHealthCheckResponse{
		Status:      status,
		Model:       model,
		CheckedAt:   checkedAt,
		Quarantined: quarantined,
		Results:     results,
	})
}

func (h *Handler) resolveHealthCheckAuth(name, authIndex string) *coreauth.Auth {
	if auth := h.authByIndex(authIndex); auth != nil {
		return auth
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if auth, ok := h.authManager.GetByID(name); ok {
		return auth
	}
	for _, auth := range h.authManager.List() {
		if auth != nil && auth.FileName == name {
			return auth
		}
	}
	return nil
}

func normalizeHealthProtocols(protocols []string) ([]healthProtocolSpec, error) {
	if len(protocols) == 0 {
		protocols = []string{"responses", "chat", "messages"}
	}
	seen := make(map[string]struct{}, len(protocols))
	result := make([]healthProtocolSpec, 0, len(protocols))
	for _, raw := range protocols {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "chat_completions" || name == "chat-completions" {
			name = "chat"
		}
		spec, ok := healthProtocolSpecs[name]
		if !ok {
			return nil, fmt.Errorf("unsupported protocol %q", raw)
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, spec)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one protocol is required")
	}
	return result, nil
}

func runAuthFileHealthProtocol(parent context.Context, baseHandler *apiHandlers.BaseAPIHandler, authID, model string, spec healthProtocolSpec) authFileHealthProtocolResult {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(parent, healthCheckTimeout)
	defer cancel()
	ctx = apiHandlers.WithPinnedAuthID(ctx, authID)

	body, _, errMessage := baseHandler.ExecuteWithAuthManager(ctx, spec.handlerFormat, model, spec.payload(model), "")
	result := authFileHealthProtocolResult{
		Protocol:  spec.name,
		LatencyMS: time.Since(startedAt).Milliseconds(),
	}
	if errMessage != nil {
		result.Status = "error"
		result.StatusCode = errMessage.StatusCode
		if errMessage.Error != nil {
			result.Error = limitHealthText(errMessage.Error.Error())
		}
		if result.Error == "" {
			result.Error = http.StatusText(result.StatusCode)
		}
		result.ErrorType = classifyHealthError(ctx, result.StatusCode, result.Error)
		return result
	}

	reply := extractHealthReply(spec.name, body)
	if reply == "" {
		result.Status = "error"
		result.Error = "model returned no assistant text"
		result.ErrorType = "empty_response"
		return result
	}
	result.Status = "success"
	result.StatusCode = http.StatusOK
	result.Reply = limitHealthText(reply)
	return result
}

func extractHealthReply(protocol string, body []byte) string {
	switch protocol {
	case "responses":
		if value := strings.TrimSpace(gjson.GetBytes(body, "output_text").String()); value != "" {
			return value
		}
		for _, output := range gjson.GetBytes(body, "output").Array() {
			for _, content := range output.Get("content").Array() {
				if value := strings.TrimSpace(content.Get("text").String()); value != "" {
					return value
				}
			}
		}
	case "chat":
		return strings.TrimSpace(gjson.GetBytes(body, "choices.0.message.content").String())
	case "messages":
		for _, content := range gjson.GetBytes(body, "content").Array() {
			if value := strings.TrimSpace(content.Get("text").String()); value != "" {
				return value
			}
		}
	}
	return ""
}

func classifyHealthError(ctx context.Context, statusCode int, message string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	lower := strings.ToLower(message)
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden ||
		strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh token") ||
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "invalid api key") || strings.Contains(lower, "expired credentials") {
		return "credential"
	}
	if statusCode == http.StatusTooManyRequests || strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") {
		return "rate_limit"
	}
	if statusCode >= http.StatusInternalServerError {
		return "upstream"
	}
	return "protocol"
}

func summarizeHealthCheckStatus(results []authFileHealthProtocolResult) string {
	succeeded := 0
	for _, result := range results {
		if result.Status == "success" {
			succeeded++
		}
	}
	switch {
	case succeeded == len(results):
		return "healthy"
	case succeeded > 0:
		return "partial"
	default:
		return "error"
	}
}

func hasCredentialHealthFailure(results []authFileHealthProtocolResult) bool {
	for _, result := range results {
		if result.ErrorType == "credential" {
			return true
		}
	}
	return false
}

func (h *Handler) persistAuthFileHealthCheck(ctx context.Context, auth *coreauth.Auth, model, status string, checkedAt time.Time, results []authFileHealthProtocolResult, quarantined bool) error {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	previous := auth.Metadata[healthCheckMetadataKey]
	auth.Metadata[healthCheckMetadataKey] = map[string]any{
		"status":      status,
		"model":       model,
		"checked_at":  checkedAt,
		"quarantined": quarantined,
		"results":     results,
	}

	if status == "error" {
		message := buildHealthStatusMessage(model, results)
		if strings.TrimSpace(auth.StatusMessage) == "" || isHealthCheckStatusMessage(auth.StatusMessage, previous) {
			auth.StatusMessage = message
		}
		if quarantined {
			auth.Disabled = true
			auth.Status = coreauth.StatusDisabled
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
			auth.LastError = &coreauth.Error{
				Code:       "management_health_check",
				Message:    message,
				HTTPStatus: firstCredentialHealthStatus(results),
			}
			auth.Metadata["disabled"] = true
		}
	} else {
		if isHealthCheckStatusMessage(auth.StatusMessage, previous) {
			auth.StatusMessage = ""
		}
		if auth.LastError != nil && auth.LastError.Code == "management_health_check" {
			auth.LastError = nil
		}
	}

	auth.UpdatedAt = checkedAt
	_, errUpdate := h.authManager.Update(ctx, auth)
	return errUpdate
}

func buildHealthStatusMessage(model string, results []authFileHealthProtocolResult) string {
	details := make([]string, 0, len(results))
	for _, result := range results {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = result.Status
		}
		details = append(details, result.Protocol+": "+limitHealthText(message))
	}
	return limitHealthText(fmt.Sprintf("model health check failed (%s): %s", model, strings.Join(details, "; ")))
}

func isHealthCheckStatusMessage(message string, marker any) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if strings.HasPrefix(message, "model health check failed (") {
		return true
	}
	markerMap, ok := marker.(map[string]any)
	if !ok {
		return false
	}
	model, _ := markerMap["model"].(string)
	return model != "" && strings.HasPrefix(message, "model health check failed ("+model+"):")
}

func firstCredentialHealthStatus(results []authFileHealthProtocolResult) int {
	for _, result := range results {
		if result.ErrorType == "credential" && result.StatusCode > 0 {
			return result.StatusCode
		}
	}
	return http.StatusUnauthorized
}

func limitHealthText(value string) string {
	value = strings.TrimSpace(value)
	const maxLength = 500
	if len(value) <= maxLength {
		return value
	}
	return value[:maxLength] + "..."
}

func mustMarshalHealthPayload(value any) []byte {
	payload, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil
	}
	return payload
}

// authFileHealthCheckBatchRequest runs health checks for multiple auth files.
type authFileHealthCheckBatchRequest struct {
	Names     []string `json:"names"`
	Supplier  string   `json:"supplier"`
	Model     string   `json:"model"`
	Protocols []string `json:"protocols"`
	Limit     int      `json:"limit"`
}

// runAuthFileHealthCheckOnce executes a health check for one auth and persists the result.
func (h *Handler) runAuthFileHealthCheckOnce(ctx context.Context, auth *coreauth.Auth, model string, protocols []string) (status string, errMsg string) {
	if h == nil || h.authManager == nil || h.cfg == nil || auth == nil {
		return "error", "core auth manager unavailable"
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "error", "model is required"
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return "error", "auth file is disabled"
	}
	specs, errProtocols := normalizeHealthProtocols(protocols)
	if errProtocols != nil {
		return "error", errProtocols.Error()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseHandler := apiHandlers.NewBaseAPIHandlers(&h.cfg.SDKConfig, h.authManager)
	results := make([]authFileHealthProtocolResult, len(specs))
	authID := auth.ID
	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = runAuthFileHealthProtocol(ctx, baseHandler, authID, model, specs[index])
		}(i)
	}
	wg.Wait()
	if latestAuth, ok := h.authManager.GetByID(authID); ok {
		auth = latestAuth
	}
	checkedAt := time.Now().UTC()
	status = summarizeHealthCheckStatus(results)
	quarantined := status == "error" && hasCredentialHealthFailure(results)
	if errPersist := h.persistAuthFileHealthCheck(ctx, auth, model, status, checkedAt, results, quarantined); errPersist != nil {
		return "error", fmt.Sprintf("failed to persist health check: %v", errPersist)
	}
	return status, ""
}

// HealthCheckAuthFilesBatch health-checks many auth files with bounded concurrency.
func (h *Handler) HealthCheckAuthFilesBatch(c *gin.Context) {
	if h == nil || h.authManager == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req authFileHealthCheckBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	names := make([]string, 0, len(req.Names))
	seen := map[string]struct{}{}
	for _, n := range req.Names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	if len(names) == 0 {
		supplier := strings.TrimSpace(req.Supplier)
		for _, auth := range h.authManager.List() {
			if auth == nil || auth.Disabled {
				continue
			}
			name := strings.TrimSpace(auth.FileName)
			if name == "" {
				continue
			}
			if supplier != "" {
				sup := ""
				if auth.Attributes != nil {
					sup = strings.TrimSpace(auth.Attributes["supplier"])
				}
				if sup == "" && auth.Metadata != nil {
					if raw, ok := auth.Metadata["supplier"].(string); ok {
						sup = strings.TrimSpace(raw)
					}
				}
				if !strings.EqualFold(sup, supplier) {
					continue
				}
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if len(names) > limit {
		names = names[:limit]
	}

	type itemResult struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Model  string `json:"model,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]itemResult, 0, len(names))
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			auth := h.resolveHealthCheckAuth(name, "")
			if auth == nil {
				mu.Lock()
				results = append(results, itemResult{Name: name, Status: "error", Error: "auth not found"})
				mu.Unlock()
				return
			}
			model := strings.TrimSpace(req.Model)
			if model == "" {
				model = firstModelForAuth(auth)
			}
			if model == "" {
				mu.Lock()
				results = append(results, itemResult{Name: name, Status: "error", Error: "no model available"})
				mu.Unlock()
				return
			}
			status, errMsg := h.runAuthFileHealthCheckOnce(c.Request.Context(), auth, model, req.Protocols)
			item := itemResult{Name: name, Status: status, Model: model}
			if errMsg != "" {
				item.Error = errMsg
			}
			mu.Lock()
			results = append(results, item)
			mu.Unlock()
		}()
	}
	wg.Wait()

	okN, failN := 0, 0
	for _, r := range results {
		switch r.Status {
		case "success", "ok", "healthy", "partial":
			okN++
		default:
			if r.Status != "skipped" {
				failN++
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"count":   len(results),
		"success": okN,
		"failed":  failN,
		"results": results,
	})
}

func firstModelForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	reg := registry.GetGlobalRegistry()
	if reg == nil {
		return ""
	}
	for _, m := range reg.GetModelsForClient(auth.ID) {
		if m == nil {
			continue
		}
		if id := strings.TrimSpace(m.ID); id != "" {
			return id
		}
	}
	return ""
}
