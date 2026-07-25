package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPatchAuthFileQuotaStatus_QuarantinesCredentialFailure(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "xai-1", FileName: "xai-1.json", Provider: "xai"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/v0/management/auth-files/quota-status",
		strings.NewReader(`{"name":"xai-1.json","success":false,"message":"401 Invalid or expired credentials","status_code":401}`),
	)
	h.PatchAuthFileQuotaStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled {
		t.Fatalf("auth should be quarantined: disabled=%v status=%q", updated.Disabled, updated.Status)
	}
	if updated.StatusMessage != "quota refresh failed: 401 Invalid or expired credentials" {
		t.Fatalf("status message = %q", updated.StatusMessage)
	}
	if _, ok := updated.Metadata[quotaCheckMetadataKey]; !ok {
		t.Fatal("expected quota check marker in metadata")
	}
}

func TestPatchAuthFileQuotaStatus_KeepsTransientFailureRoutable(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "xai-2", FileName: "xai-2.json", Provider: "xai"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/v0/management/auth-files/quota-status",
		strings.NewReader(`{"name":"xai-2.json","success":false,"message":"EOF","status_code":502}`),
	)
	h.PatchAuthFileQuotaStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled || updated.StatusMessage != "quota refresh failed: EOF" {
		t.Fatalf("transient failure state = disabled=%v message=%q", updated.Disabled, updated.StatusMessage)
	}
}

func TestPatchAuthFileQuotaStatus_SuccessClearsItsOwnMarker(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "xai-3", FileName: "xai-3.json", Provider: "xai"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	mark := func(body string) {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/quota-status", strings.NewReader(body))
		h.PatchAuthFileQuotaStatus(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	mark(`{"name":"xai-3.json","success":false,"message":"EOF","status_code":502}`)
	mark(`{"name":"xai-3.json","success":true}`)

	updated, _ := manager.GetByID(auth.ID)
	if updated.StatusMessage != "" {
		t.Fatalf("status message = %q, want empty", updated.StatusMessage)
	}
	if _, ok := updated.Metadata[quotaCheckMetadataKey]; ok {
		t.Fatal("expected quota check marker to be removed")
	}
}
