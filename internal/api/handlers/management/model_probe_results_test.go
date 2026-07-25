package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestModelProbeResultsPersistPerCredentialModelProtocol(t *testing.T) {
	authDir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{
		ID:       "auth-a",
		Index:    "index-a",
		Provider: "antigravity",
	}
	firstCheckedAt := time.Date(2026, 7, 25, 1, 0, 0, 0, time.UTC)
	if errPersist := handler.persistModelProbeResults(context.Background(), auth, "claude-opus", firstCheckedAt, []authFileHealthProtocolResult{
		{Protocol: "responses", Status: "error", StatusCode: 503, LatencyMS: 1200, Error: "capacity exhausted", ErrorType: "upstream"},
		{Protocol: "messages", Status: "success", StatusCode: 200, LatencyMS: 800},
	}); errPersist != nil {
		t.Fatalf("persistModelProbeResults() error = %v", errPersist)
	}

	secondCheckedAt := firstCheckedAt.Add(time.Minute)
	if errPersist := handler.persistModelProbeResults(context.Background(), auth, "claude-opus", secondCheckedAt, []authFileHealthProtocolResult{
		{Protocol: "responses", Status: "success", StatusCode: 200, LatencyMS: 700},
	}); errPersist != nil {
		t.Fatalf("persistModelProbeResults() second error = %v", errPersist)
	}

	reloaded := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	results, errLoad := reloaded.loadModelProbeResultsLocked(context.Background(), secondCheckedAt)
	if errLoad != nil {
		t.Fatalf("loadModelProbeResultsLocked() error = %v", errLoad)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	byProtocol := make(map[string]modelProbeResult, len(results))
	for _, result := range results {
		byProtocol[result.Protocol] = result
	}
	responses := byProtocol["responses"]
	if responses.Status != "success" || !responses.CheckedAt.Equal(secondCheckedAt) {
		t.Fatalf("responses result = %#v, want latest success", responses)
	}
	messages := byProtocol["messages"]
	if messages.Status != "success" || !messages.CheckedAt.Equal(firstCheckedAt) {
		t.Fatalf("messages result = %#v, want original success", messages)
	}
}

func TestModelProbeResultsDropExpiredEvidence(t *testing.T) {
	authDir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{ID: "auth-a", Index: "index-a", Provider: "codex"}
	now := time.Date(2026, 7, 25, 2, 0, 0, 0, time.UTC)
	if errPersist := handler.persistModelProbeResults(context.Background(), auth, "old-model", now.Add(-31*24*time.Hour), []authFileHealthProtocolResult{
		{Protocol: "responses", Status: "error"},
	}); errPersist != nil {
		t.Fatalf("persist old result error = %v", errPersist)
	}
	if errPersist := handler.persistModelProbeResults(context.Background(), auth, "new-model", now, []authFileHealthProtocolResult{
		{Protocol: "responses", Status: "success"},
	}); errPersist != nil {
		t.Fatalf("persist new result error = %v", errPersist)
	}

	results, errLoad := handler.loadModelProbeResultsLocked(context.Background(), now)
	if errLoad != nil {
		t.Fatalf("loadModelProbeResultsLocked() error = %v", errLoad)
	}
	if len(results) != 1 || results[0].Model != "new-model" {
		t.Fatalf("results = %#v, want only new-model", results)
	}
}

func TestListModelProbeResultsReturnsPersistedEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{ID: "auth-a", Index: "index-a", Provider: "gemini"}
	checkedAt := time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)
	if errPersist := handler.persistModelProbeResults(context.Background(), auth, "gemini-pro", checkedAt, []authFileHealthProtocolResult{
		{Protocol: "chat", Status: "success", StatusCode: http.StatusOK, LatencyMS: 321},
	}); errPersist != nil {
		t.Fatalf("persistModelProbeResults() error = %v", errPersist)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/auth-files/model-probe-results", nil)
	handler.ListModelProbeResults(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Results       []modelProbeResult `json:"results"`
		RetentionDays int                `json:"retention_days"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.RetentionDays != 30 {
		t.Fatalf("retention_days = %d, want 30", response.RetentionDays)
	}
	if len(response.Results) != 1 || response.Results[0].AuthIndex != "index-a" || response.Results[0].Model != "gemini-pro" {
		t.Fatalf("results = %#v, want persisted evidence", response.Results)
	}
}
