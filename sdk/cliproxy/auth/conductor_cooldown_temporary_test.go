package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestTemporary403CooldownSuspendsModelThenRecovers verifies the cooldown contract for a
// single 403 failure under upstream's model-projection rules: the model enters a temporary
// model-level cooldown, the registry reports that cooling model as suspended for its client
// (upstream's ApplyClientModelProjections deliberately hides any model with a live cooldown,
// so the model count drops to 0 while the cooldown lasts), and both the cooldown and the
// suspension clear once the cooldown expires.
//
// This intentionally asserts upstream's behaviour. The earlier local expectation was that a
// temporary 403 leaves the model counted in the registry; upstream now projects live model
// cooldowns as suspended, and that projection wins. The suspension is therefore a temporary
// follow-the-cooldown state, not a permanent registry removal.
func TestTemporary403CooldownSuspendsModelThenRecovers(t *testing.T) {
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

	// While the cooldown is live, upstream's projection reports the model as suspended for
	// this client, so it is not counted as an available provider of the model.
	if !reg.IsModelSuspendedForClient(auth.ID, model) {
		t.Fatal("expected the cooling model to be projected as suspended for its client")
	}
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("GetModelCount = %d, want 0 while the model cooldown is live", count)
	}

	// Expire the cooldown without clearing the state: reconciliation must treat the elapsed
	// NextRetryAfter as recovered and re-project the model as available.
	m.mu.Lock()
	if a, ok := m.auths[auth.ID]; ok && a != nil {
		if s, ok := a.ModelStates[model]; ok && s != nil {
			s.NextRetryAfter = time.Now().Add(-1 * time.Second)
		}
	}
	m.mu.Unlock()
	m.ReconcileRegistryModelStates(context.Background(), auth.ID)

	if reg.IsModelSuspendedForClient(auth.ID, model) {
		t.Fatal("expected the suspension to clear once the cooldown expired")
	}
	if count := reg.GetModelCount(model); count != 1 {
		t.Fatalf("GetModelCount = %d, want 1 after the cooldown expired", count)
	}
	recovered, ok := m.GetByID(auth.ID)
	if !ok || recovered == nil {
		t.Fatal("expected auth to remain registered after recovery")
	}
	if recoveredState := recovered.ModelStates[model]; recoveredState != nil && recoveredState.Unavailable {
		t.Fatalf("expected the model cooldown to be cleared, got %#v", recoveredState)
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
