package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	opencodeUsageURL            = "https://opencode.ai/zen/go/v1/usage"
	opencodeCookiePrefix        = "auth="
	opencodeAuthFileProvider    = "opencode"
	defaultOpencodeUsageTimeout = 20 * time.Second
)

// opencodeUsageWindow describes one OpenCode Go usage window (rolling/weekly/monthly).
type opencodeUsageWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

type opencodeUsage struct {
	Rolling *opencodeUsageWindow `json:"rolling"`
	Weekly  *opencodeUsageWindow `json:"weekly"`
	Monthly *opencodeUsageWindow `json:"monthly"`
}

type opencodeUsageResponse struct {
	CookieSet bool           `json:"cookie_set"`
	APIKeySet bool           `json:"api_key_set"`
	Usage     *opencodeUsage `json:"usage,omitempty"`
	Raw       string         `json:"raw,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type opencodeUsageRequest struct {
	// Name selects a saved opencode auth file by its auth-file name (e.g. opencode-go-...json).
	Name string `json:"name"`
	// Cookie is the opencode session cookie request header (auth=Fe26.2**...).
	Cookie string `json:"cookie"`
	// APIKey is the opencode Zen API key (sk-...).
	APIKey string `json:"api_key"`
	// WorkspaceID is the wrk_... workspace scope, echoed back for display.
	WorkspaceID string `json:"workspace_id"`
}

// GetOpenCodeUsage proxies OpenCode Go usage for the selected credential.
//
// Endpoint:
//
//	POST /v0/management/opencode/usage  (JSON body, same fields as the request struct)
//	GET  /v0/management/opencode/usage  (query params: name, cookie, api_key, workspace_id)
//
// Response JSON:
//   - cookie_set / api_key_set: which credentials were available.
//   - usage.rolling / weekly / monthly: {status, percent, resetsAt}.
//   - raw: the upstream JSON body.
//   - error: human-readable error when the request could not be resolved.
func (h *Handler) GetOpenCodeUsage(c *gin.Context) {
	ctx := c.Request.Context()

	var req opencodeUsageRequest
	if c.Request.Method == http.MethodPost {
		if errBind := c.ShouldBindJSON(&req); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	if q := strings.TrimSpace(c.Query("name")); q != "" {
		req.Name = q
	}
	if q := strings.TrimSpace(c.Query("cookie")); q != "" {
		req.Cookie = q
	}
	if q := strings.TrimSpace(c.Query("api_key")); q != "" {
		req.APIKey = q
	}
	if q := strings.TrimSpace(c.Query("workspace_id")); q != "" {
		req.WorkspaceID = q
	}

	auth := h.opencodeAuthRecord(req.Name)
	cookie, apiKey := opencodeCredentials(auth, req)

	if cookie == "" && apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no opencode credential available"})
		return
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, opencodeUsageResponse{
			CookieSet: cookie != "",
			APIKeySet: false,
			Error:     "missing OpenCode Zen API Key (AK): add an sk-... key to query credits",
		})
		return
	}

	usage, rawBody, err := h.fetchOpencodeUsage(ctx, apiKey)
	if err != nil {
		log.WithError(err).Debug("opencode usage fetch failed")
		c.JSON(http.StatusBadGateway, opencodeUsageResponse{
			CookieSet: cookie != "",
			APIKeySet: apiKey != "",
			Raw:       rawBody,
			Error:     err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, opencodeUsageResponse{
		CookieSet: cookie != "",
		APIKeySet: apiKey != "",
		Usage:     usage,
		Raw:       rawBody,
	})
}

func (h *Handler) opencodeAuthRecord(name string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	var first *coreauth.Auth
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), opencodeAuthFileProvider) {
			continue
		}
		if first == nil {
			first = auth
		}
		if name != "" && strings.EqualFold(strings.TrimSpace(auth.FileName), name) {
			return auth
		}
	}
	return first
}

func opencodeCredentials(auth *coreauth.Auth, req opencodeUsageRequest) (cookie, apiKey string) {
	cookie = strings.TrimSpace(req.Cookie)
	apiKey = strings.TrimSpace(req.APIKey)

	if auth == nil {
		return cookie, apiKey
	}
	if cookie == "" {
		cookie = opencodeCookieFromAuth(auth)
	}
	if apiKey == "" {
		apiKey = opencodeAPIKeyFromAuth(auth)
	}
	return cookie, apiKey
}

func opencodeCookieFromAuth(auth *coreauth.Auth) string {
	if auth == nil || len(auth.Metadata) == 0 {
		return ""
	}
	for _, key := range []string{"cookie", "auth", "Cookie", "Auth"} {
		if v := strings.TrimSpace(stringValue(auth.Metadata, key)); v != "" {
			return ensureOpencodeAuthPrefix(v)
		}
	}
	return ""
}

func opencodeAPIKeyFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if len(auth.Metadata) > 0 {
		if v := strings.TrimSpace(stringValue(auth.Metadata, "api_key")); v != "" {
			return v
		}
		if v := strings.TrimSpace(stringValue(auth.Metadata, "apiKey")); v != "" {
			return v
		}
	}
	if auth.Attributes != nil {
		return strings.TrimSpace(auth.Attributes["api_key"])
	}
	return ""
}

func ensureOpencodeAuthPrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, opencodeCookiePrefix) {
		return value
	}
	return opencodeCookiePrefix + value
}

func (h *Handler) fetchOpencodeUsage(ctx context.Context, apiKey string) (*opencodeUsage, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, opencodeUsageURL, nil)
	if errReq != nil {
		return nil, "", fmt.Errorf("failed to build opencode usage request: %w", errReq)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	httpClient := &http.Client{Timeout: defaultOpencodeUsageTimeout}
	httpClient.Transport = h.apiCallTransport(nil, "")

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, "", fmt.Errorf("opencode usage request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("opencode usage response close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, "", fmt.Errorf("failed to read opencode usage response: %w", errRead)
	}
	rawBody := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		return nil, rawBody, fmt.Errorf("opencode usage returned HTTP %d: %s", resp.StatusCode, truncateOpencodeBody(rawBody))
	}

	var wrapper struct {
		Usage *opencodeUsage `json:"usage"`
		Type  string         `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &wrapper); errUnmarshal != nil {
		return nil, rawBody, fmt.Errorf("failed to decode opencode usage response: %w", errUnmarshal)
	}
	if wrapper.Type == "error" && wrapper.Error != nil {
		return nil, rawBody, fmt.Errorf("opencode usage error: %s", wrapper.Error.Message)
	}
	if wrapper.Usage == nil || (wrapper.Usage.Rolling == nil && wrapper.Usage.Weekly == nil && wrapper.Usage.Monthly == nil) {
		return nil, rawBody, fmt.Errorf("opencode usage response missing usage")
	}

	return wrapper.Usage, rawBody, nil
}

func truncateOpencodeBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) <= 400 {
		return body
	}
	return body[:400] + "..."
}
