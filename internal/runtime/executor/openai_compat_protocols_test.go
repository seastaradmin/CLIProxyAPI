package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatUpstreamFormat_Passthrough(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:      "MIMO",
			BaseURL:   "https://example.com/v1",
			Protocols: []string{"chat", "responses", "messages"},
			Models:    []config.OpenAICompatibilityModel{{Name: "mimo-v2.5"}},
		}},
	}
	e := NewOpenAICompatExecutor("openai-compatible-mimo", cfg)
	auth := &cliproxyauth.Auth{
		Label: "MIMO",
		Attributes: map[string]string{
			"compat_name": "MIMO",
			"protocols":   "chat,responses,messages",
		},
	}
	req := cliproxyexecutor.Request{Model: "mimo-v2.5", Payload: []byte(`{"model":"mimo-v2.5"}`)}
	cases := []struct {
		source, want, path string
	}{
		{"openai-response", "openai-response", "/responses"},
		{"claude", "claude", "/messages"},
		{"openai", "openai", "/chat/completions"},
	}
	for _, tc := range cases {
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.source)}
		got := e.openAICompatUpstreamFormat(auth, req, opts)
		if got.String() != tc.want {
			t.Fatalf("source %s: to=%s want %s", tc.source, got.String(), tc.want)
		}
		if path := openAICompatEndpoint(got, ""); path != tc.path {
			t.Fatalf("source %s: path=%s want %s", tc.source, path, tc.path)
		}
	}
}

func TestOpenAICompatUpstreamFormat_DefaultChatOnly(t *testing.T) {
	e := NewOpenAICompatExecutor("openai-compatible-x", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"compat_name": "x"}}
	req := cliproxyexecutor.Request{Model: "gpt", Payload: []byte(`{}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
	got := e.openAICompatUpstreamFormat(auth, req, opts)
	if got.String() != "openai" {
		t.Fatalf("default fallback to=%s want openai", got.String())
	}
}
