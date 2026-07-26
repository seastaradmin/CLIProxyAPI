package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// Fixture: one provider with a channel attribute of "chat", a provider-level
// model marked [responses], and two key entries whose per-key lists disagree.
func perKeyProtocolFixture() (*OpenAICompatExecutor, *cliproxyauth.Auth) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "acme",
			BaseURL: "https://acme.example/v1",
			Models: []config.OpenAICompatibilityModel{{
				Name:      "m1",
				Protocols: []string{"responses"},
			}},
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{
					APIKey: "key-a",
					Models: []config.OpenAICompatibilityModel{{
						Name:      "m1",
						Protocols: []string{"messages"},
						ProtocolEndpoints: map[string]string{
							"messages": "/anthropic/v1/messages",
						},
					}},
				},
				{
					APIKey: "key-b",
					Models: []config.OpenAICompatibilityModel{{
						Name:      "m2",
						Alias:     "alias-2",
						Protocols: []string{"responses", "chat"},
					}},
				},
			},
		}},
	}
	e := NewOpenAICompatExecutor("openai-compatible-acme", cfg)
	auth := &cliproxyauth.Auth{
		Label: "acme",
		Attributes: map[string]string{
			"compat_name": "acme",
			"base_url":    "https://acme.example/v1",
			"api_key":     "key-a",
			"protocols":   "chat",
		},
	}
	return e, auth
}

func TestOpenAICompatProtocols_KeyEntryOverridesChannelAttribute(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	req := cliproxyexecutor.Request{Model: "m1"}
	got := e.openAICompatProtocols(auth, req, cliproxyexecutor.Options{})
	if len(got) != 1 || got[0] != "messages" {
		t.Fatalf("protocols = %v, want [messages] (owning key entry wins)", got)
	}
}

func TestOpenAICompatProtocols_ProviderLevelWhenKeyHasNoMatch(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	// key-b owns alias-2; probing m1 via key-b should fall back to the
	// provider-level model list.
	auth.Attributes["api_key"] = "key-b"
	req := cliproxyexecutor.Request{Model: "m1"}
	got := e.openAICompatProtocols(auth, req, cliproxyexecutor.Options{})
	if len(got) != 1 || got[0] != "responses" {
		t.Fatalf("protocols = %v, want [responses] (provider-level fallback)", got)
	}
}

func TestOpenAICompatProtocols_OtherKeyEntryAsLastModelSource(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	// alias-2 lives only on key-b; requesting it via key-a should still find
	// the model-level protocols from the other entry.
	req := cliproxyexecutor.Request{Model: "alias-2"}
	got := e.openAICompatProtocols(auth, req, cliproxyexecutor.Options{})
	if len(got) != 2 || got[0] != "responses" || got[1] != "chat" {
		t.Fatalf("protocols = %v, want [responses chat] (other key entry fallback)", got)
	}
}

func TestOpenAICompatProtocols_ChannelAttributeFallback(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	req := cliproxyexecutor.Request{Model: "unknown-model"}
	got := e.openAICompatProtocols(auth, req, cliproxyexecutor.Options{})
	if len(got) != 1 || got[0] != "chat" {
		t.Fatalf("protocols = %v, want [chat] (channel attribute fallback)", got)
	}
}

func TestOpenAICompatEndpointResolved_PerKeyStoredEndpoint(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	req := cliproxyexecutor.Request{Model: "m1"}
	to := sdktranslator.FromString("claude")
	if got := e.openAICompatEndpointResolved(auth, req, to, ""); got != "/anthropic/v1/messages" {
		t.Fatalf("endpoint = %q, want stored per-key path", got)
	}
}

func TestOpenAICompatEndpointResolved_DefaultWhenNoArchive(t *testing.T) {
	e, auth := perKeyProtocolFixture()
	req := cliproxyexecutor.Request{Model: "m1"}
	to := sdktranslator.FromString("openai-response")
	if got := e.openAICompatEndpointResolved(auth, req, to, ""); got != "/responses" {
		t.Fatalf("endpoint = %q, want default /responses", got)
	}
}
