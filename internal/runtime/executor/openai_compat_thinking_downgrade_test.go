package executor

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestExecuteStream_DowngradesAdaptiveThinkingOnTypeRejection verifies that a
// 400 rejecting adaptive thinking.type (e.g. middleboxes accepting only
// enabled/disabled/auto) triggers one retry with the legacy thinking form.
func TestExecuteStream_DowngradesAdaptiveThinkingOnTypeRejection(t *testing.T) {
	var calls, retriedBodies int32
	var firstBody, secondBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if atomic.AddInt32(&calls, 1) == 1 {
			firstBody = body
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_parameter_error","param":null,"message":"'type' must be in [\"enabled\", \"disabled\", \"auto\"]","type":"invalid_request_error"}}`))
			return
		}
		secondBody = body
		atomic.AddInt32(&retriedBodies, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"m-1","model":"demo"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	defer server.Close()

	e := NewOpenAICompatExecutor("openai-compatible-thinking", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:      "thinking-downgrade",
			Protocols: []string{"messages"},
			Models:    []config.OpenAICompatibilityModel{{Name: "demo"}},
		}},
	})
	auth := &cliproxyauth.Auth{
		Label: "thinking-downgrade",
		Attributes: map[string]string{
			"compat_name": "thinking-downgrade",
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	payload := []byte(`{"model":"demo","max_tokens":32000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "demo", Payload: payload}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude"), ResponseFormat: sdktranslator.FromString("claude")}

	result, err := e.ExecuteStream(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var got []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		got = append(got, chunk.Payload...)
	}
	if !bytes.Contains(got, []byte("content_block_delta")) {
		t.Fatalf("stream payload missing content: %q", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original + downgraded retry)", got)
	}
	if gotType := gjson.GetBytes(firstBody, "thinking.type").String(); gotType != "adaptive" {
		t.Fatalf("first body thinking.type = %q, want adaptive", gotType)
	}
	if gotType := gjson.GetBytes(secondBody, "thinking.type").String(); gotType != "enabled" {
		t.Fatalf("retried body thinking.type = %q, want enabled", gotType)
	}
	if gotBudget := gjson.GetBytes(secondBody, "thinking.budget_tokens").Int(); gotBudget != 24576 {
		t.Fatalf("retried body budget_tokens = %d, want 24576 (high)", gotBudget)
	}
	if gjson.GetBytes(secondBody, "output_config.effort").Exists() {
		t.Fatalf("retried body must not carry output_config.effort")
	}
}

// TestExecuteStream_NoDowngradeRetryForOther400s verifies that unrelated 400
// errors do not trigger the thinking downgrade path.
func TestExecuteStream_NoDowngradeRetryForOther400s(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"messages[1]: unknown field"}}`))
	}))
	defer server.Close()

	e := NewOpenAICompatExecutor("openai-compatible-thinking", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:      "thinking-nodowngrade",
			Protocols: []string{"messages"},
			Models:    []config.OpenAICompatibilityModel{{Name: "demo"}},
		}},
	})
	auth := &cliproxyauth.Auth{
		Label: "thinking-nodowngrade",
		Attributes: map[string]string{
			"compat_name": "thinking-nodowngrade",
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	payload := []byte(`{"model":"demo","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "demo", Payload: payload}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude"), ResponseFormat: sdktranslator.FromString("claude")}

	if _, err := e.ExecuteStream(t.Context(), auth, req, opts); err == nil {
		t.Fatal("expected error for unrelated 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no downgrade retry for unrelated 400)", got)
	}
}
