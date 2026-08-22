package management

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const quotaCheckMetadataKey = "management_quota_check"

// PatchAuthFileQuotaStatus persists the result of a management-center quota check.
// Credential failures are quarantined so they cannot be selected by normal routing
// until an operator re-enables or deletes the auth file.
func (h *Handler) PatchAuthFileQuotaStatus(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name       string `json:"name"`
		Success    bool   `json:"success"`
		Message    string `json:"message"`
		StatusCode int    `json:"status_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	targetAuth, _ := h.lookupAuthFile(name, "")
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}

	if targetAuth.Metadata == nil {
		targetAuth.Metadata = make(map[string]any)
	}
	if req.Success {
		clearQuotaCheckStatus(targetAuth)
	} else {
		message := strings.TrimSpace(req.Message)
		if kind := coreauth.ClassifyUsageError(req.StatusCode, "", message); kind != "" {
			h.authManager.MarkResult(c.Request.Context(), coreauth.Result{
				AuthID: targetAuth.ID,
				Error: &coreauth.Error{
					Code:       string(kind),
					Message:    message,
					HTTPStatus: http.StatusTooManyRequests,
				},
			})
			if latest, ok := h.authManager.GetByID(targetAuth.ID); ok && latest != nil {
				targetAuth = latest
			}
		}
		applyQuotaCheckFailure(targetAuth, req.Message, req.StatusCode)
	}
	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"success":     req.Success,
		"quarantined": targetAuth.Disabled,
	})
}

func applyQuotaCheckFailure(auth *coreauth.Auth, message string, statusCode int) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "quota refresh failed"
	}
	quarantined := isQuotaCredentialFailure(message, statusCode)
	auth.Metadata[quotaCheckMetadataKey] = map[string]any{
		"message":     message,
		"status_code": statusCode,
		"quarantined": quarantined,
	}

	quotaMessage := "quota refresh failed: " + message
	if auth.StatusMessage == "" || isQuotaCheckStatusMessage(auth.StatusMessage, auth.Metadata[quotaCheckMetadataKey]) {
		auth.StatusMessage = quotaMessage
	}
	if coreauth.ClassifyUsageError(statusCode, "", message) != "" {
		return
	}
	if !quarantined {
		return
	}

	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.LastError = &coreauth.Error{
		Code:       "management_quota_check",
		Message:    message,
		HTTPStatus: statusCode,
	}
	auth.Metadata["disabled"] = true
}

func clearQuotaCheckStatus(auth *coreauth.Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	marker, ok := auth.Metadata[quotaCheckMetadataKey].(map[string]any)
	if !ok {
		delete(auth.Metadata, quotaCheckMetadataKey)
		return
	}
	delete(auth.Metadata, quotaCheckMetadataKey)
	if isQuotaCheckStatusMessage(auth.StatusMessage, marker) {
		auth.StatusMessage = ""
	}
	if auth.LastError != nil && auth.LastError.Code == "management_quota_check" {
		auth.LastError = nil
	}
}

func isQuotaCheckStatusMessage(message string, marker any) bool {
	message = strings.TrimSpace(message)
	markerMap, ok := marker.(map[string]any)
	if !ok {
		return strings.HasPrefix(message, "quota refresh failed:")
	}
	previous, _ := markerMap["message"].(string)
	return message == "quota refresh failed: "+strings.TrimSpace(previous)
}

func isQuotaCredentialFailure(message string, statusCode int) bool {
	if coreauth.ClassifyUsageError(statusCode, "", message) != "" {
		return false
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return true
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{"invalid_grant", "invalid or expired credentials", "refresh token", "unauthorized", "forbidden"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
