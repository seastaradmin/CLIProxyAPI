package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestChannelDisabled_ExcludesProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()

	claudeAuth := &Auth{
		ID:       "claude-1",
		Provider: "claude",
		Status:   StatusActive,
	}
	codexAuth := &Auth{
		ID:       "codex-1",
		Provider: "codex",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, claudeAuth); err != nil {
		t.Fatalf("register claude: %v", err)
	}
	if _, err := manager.Register(ctx, codexAuth); err != nil {
		t.Fatalf("register codex: %v", err)
	}

	// Disable the codex channel.
	manager.SetConfig(&internalconfig.Config{
		OAuthDisabledChannels: map[string]bool{"codex": true},
	})

	providers := manager.AvailableProviders()
	for _, p := range providers {
		if p == "codex" {
			t.Fatalf("codex should not be in AvailableProviders when channel is disabled")
		}
	}
	found := false
	for _, p := range providers {
		if p == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatalf("claude should still be in AvailableProviders")
	}

	if manager.HasProviderAuth("codex") {
		t.Fatalf("HasProviderAuth(codex) should be false when channel is disabled")
	}
	if !manager.HasProviderAuth("claude") {
		t.Fatalf("HasProviderAuth(claude) should still be true")
	}

	// Re-enable codex channel.
	manager.SetConfig(&internalconfig.Config{
		OAuthDisabledChannels: map[string]bool{},
	})
	if !manager.HasProviderAuth("codex") {
		t.Fatalf("HasProviderAuth(codex) should be true after re-enabling channel")
	}
}

func TestChannelDisabled_AvailableProvidersEmpty(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()

	if _, err := manager.Register(ctx, &Auth{
		ID:       "codex-1",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.SetConfig(&internalconfig.Config{
		OAuthDisabledChannels: map[string]bool{"codex": true},
	})

	if len(manager.AvailableProviders()) != 0 {
		t.Fatalf("expected empty AvailableProviders, got %v", manager.AvailableProviders())
	}
}
