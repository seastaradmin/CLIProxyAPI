package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestConfigSynthesizer_OpenAICompatPerKeyModels(t *testing.T) {
	s := &ConfigSynthesizer{}
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "mint",
				BaseURL: "https://example.com/v1",
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared-a", Alias: "shared-a"},
					{Name: "shared-b", Alias: "shared-b"},
				},
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{
						APIKey: "key-a",
						Models: []config.OpenAICompatibilityModel{
							{Name: "only-a", Alias: "only-a"},
						},
					},
					{
						APIKey: "key-b",
						// inherit provider models
					},
				},
			},
		},
	}
	ctx := &SynthesisContext{
		Config:      cfg,
		AuthDir:     t.TempDir(),
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths := s.synthesizeOpenAICompat(ctx)
	if len(auths) != 2 {
		t.Fatalf("auths = %d, want 2", len(auths))
	}
	var keyA, keyB *coreauthAuth
	for _, a := range auths {
		if a.Attributes["api_key"] == "key-a" {
			keyA = &coreauthAuth{hash: a.Attributes["models_hash"], override: a.Attributes["models_override"]}
		}
		if a.Attributes["api_key"] == "key-b" {
			keyB = &coreauthAuth{hash: a.Attributes["models_hash"], override: a.Attributes["models_override"]}
		}
	}
	if keyA == nil || keyB == nil {
		t.Fatalf("missing keys: a=%v b=%v", keyA, keyB)
	}
	if keyA.override != "1" {
		t.Fatalf("key-a models_override = %q, want 1", keyA.override)
	}
	if keyB.override != "" {
		t.Fatalf("key-b models_override = %q, want empty", keyB.override)
	}
	if keyA.hash == "" || keyB.hash == "" {
		t.Fatalf("missing models_hash a=%q b=%q", keyA.hash, keyB.hash)
	}
	if keyA.hash == keyB.hash {
		t.Fatalf("expected different models_hash for per-key override, both %q", keyA.hash)
	}
}

type coreauthAuth struct {
	hash     string
	override string
}
