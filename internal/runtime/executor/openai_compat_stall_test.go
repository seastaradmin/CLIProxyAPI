package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestExecuteStream_StallTimeoutFailsAsRetryable verifies that a stream which
// starts (first bytes delivered) and then goes completely silent is declared a
// dead stream and fails with a retryable deadline error instead of hanging or
// truncating.
func TestExecuteStream_StallTimeoutFailsAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		// First bytes arrive, then the upstream goes completely silent and
		// never closes the connection.
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-1\",\"model\":\"demo\"}}\n\n"))
		flusher.Flush()
		// Hold the connection open without sending anything more; the request
		// context cancels when the client gives up / closes the body.
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{Streaming: config.StreamingConfig{StallTimeoutSeconds: 1}}}
	e := NewOpenAICompatExecutor("openai-compatible-stall", cfg)
	auth := &cliproxyauth.Auth{
		Label: "stall",
		Attributes: map[string]string{
			"compat_name": "stall",
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	payload := []byte(`{"model":"demo","messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "demo", Payload: payload}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude"), ResponseFormat: sdktranslator.FromString("claude")}

	start := time.Now()
	result, err := e.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var gotErr error
	var payloads int
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			continue
		}
		payloads++
	}
	elapsed := time.Since(start)
	if gotErr == nil {
		t.Fatal("expected stall timeout error, got clean stream end")
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Fatalf("stall error = %v, want context.DeadlineExceeded", gotErr)
	}
	if payloads == 0 {
		t.Fatal("expected the initial message_start payload to be forwarded before the stall failure")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stall detection took %v, want ~1s", elapsed)
	}
	if elapsed < 800*time.Millisecond {
		t.Fatalf("stall detection fired too early: %v", elapsed)
	}
}

// TestExecuteStream_StallGuardDisabled verifies stall-timeout-seconds < 0 keeps
// the legacy behavior (wait indefinitely).
func TestExecuteStream_StallGuardDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{Streaming: config.StreamingConfig{StallTimeoutSeconds: -1}}}
	e := NewOpenAICompatExecutor("openai-compatible-stall", cfg)
	auth := &cliproxyauth.Auth{
		Label: "stall-off",
		Attributes: map[string]string{
			"compat_name": "stall-off",
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	payload := []byte(`{"model":"demo","messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "demo", Payload: payload}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude"), ResponseFormat: sdktranslator.FromString("claude")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	// Drain one payload then verify the stream keeps waiting (no stall error).
	select {
	case chunk := <-result.Chunks:
		if chunk.Err != nil {
			t.Fatalf("unexpected error with stall guard disabled: %v", chunk.Err)
		}
		if len(chunk.Payload) == 0 {
			t.Fatal("expected message_start payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no payload received")
	}
	// The stream must still be open (blocked on upstream) — a second read must
	// NOT return within the stall window when the guard is disabled.
	select {
	case chunk, ok := <-result.Chunks:
		if ok && chunk.Err != nil {
			t.Fatalf("unexpected stall error when guard disabled: %v", chunk.Err)
		}
	case <-time.After(1500 * time.Millisecond):
		// expected: still blocked, no stall error
	}
	// Cancel the request so the executor releases the upstream connection and
	// the test server can shut down.
	cancel()
	for range result.Chunks {
	}
	_ = io.EOF
}
