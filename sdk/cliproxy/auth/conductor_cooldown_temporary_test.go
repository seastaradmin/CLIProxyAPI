package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestTemporary403CooldownRecoversAfterExpiry verifies that a single 403 failure
// puts the model into a temporary cooldown that automatically recovers once the
// cooldown expires, and that the model is NOT permanently suspended in the registry.
func TestTemporary403CooldownRecoversAfterExpiry(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	hook := &resultCaptureHook{}
	m := NewManager(nil, nil, hook)
	executor := &authFallbackExecutor{
		id: "openai-compatibility",
		executeErrors: map[string]error{
			"temp-403-auth": &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "Upstream request failed: [403] This model is not available in your region.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "test-model-403"
	auth := &Auth{ID: "temp-403-auth", Provider: "openai-compatibility"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// First execute: should fail with 403 and enter cooldown.
	if _, errExec := m.Execute(context.Background(), []string{"openai-compatibility"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExec == nil {
		t.Fatal("expected 403 error on first execute")
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	state := updated.ModelStates[model]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected model cooldown state, got %#v", state)
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected NextRetryAfter to be set")
	}

	// The model must NOT be permanently suspended in the registry.
	// GetModelCount should still reflect the registered client (count=1),
	// not 0 which would indicate a registry-level suspension.
	if count := reg.GetModelCount(model); count != 1 {
		t.Fatalf("GetModelCount = %d, want 1 (temporary 403 must not permanently suspend model)", count)
	}

	// Simulate cooldown expiry by backdating NextRetryAfter and clearing the unavailable flag.
	m.mu.Lock()
	if a, ok := m.auths[auth.ID]; ok && a != nil {
		if s, ok := a.ModelStates[model]; ok && s != nil {
			s.NextRetryAfter = time.Now().Add(-1 * time.Second)
			s.Unavailable = false
		}
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(func() *Auth {
			a, _ := m.GetByID(auth.ID)
			return a
		}())
	}

	// Clear the execute error so the next call succeeds.
	executor.mu.Lock()
	delete(executor.executeErrors, "temp-403-auth")
	executor.mu.Unlock()

	// Second execute: should succeed because cooldown has expired and model is not suspended.
	resp, errExec2 := m.Execute(context.Background(), []string{"openai-compatibility"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec2 != nil {
		t.Fatalf("expected success after cooldown expiry, got: %v", errExec2)
	}
	if string(resp.Payload) != auth.ID {
		t.Fatalf("unexpected payload: %q", string(resp.Payload))
	}
}
