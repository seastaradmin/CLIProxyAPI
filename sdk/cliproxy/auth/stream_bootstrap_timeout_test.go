package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestReadStreamBootstrapWithTimeout_TimesOutWithoutPayload(t *testing.T) {
	t.Parallel()

	ch := make(chan cliproxyexecutor.StreamChunk)
	// Keep the channel open so bootstrap must wait for payload, not close.
	defer close(ch)

	start := time.Now()
	buffered, closed, err := readStreamBootstrapWithTimeout(context.Background(), ch, 40*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected first-chunk timeout error")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		t.Fatalf("error type = %T (%v), want *Error", err, err)
	}
	if authErr.Code != "first_chunk_timeout" {
		t.Fatalf("code = %q, want first_chunk_timeout", authErr.Code)
	}
	if !authErr.Retryable {
		t.Fatal("expected retryable timeout")
	}
	if authErr.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", authErr.HTTPStatus, http.StatusGatewayTimeout)
	}
	if closed {
		t.Fatal("closed = true, want false on timeout")
	}
	if len(buffered) != 0 {
		t.Fatalf("buffered = %#v, want empty", buffered)
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("elapsed = %v, timeout returned too early", elapsed)
	}
}

func TestReadStreamBootstrapWithTimeout_IgnoresEmptyFramesUntilPayload(t *testing.T) {
	t.Parallel()

	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- cliproxyexecutor.StreamChunk{Payload: nil}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte{}}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("hello")}
	close(ch)

	buffered, closed, err := readStreamBootstrapWithTimeout(context.Background(), ch, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed {
		t.Fatal("closed = true, want false after payload")
	}
	if len(buffered) != 3 {
		t.Fatalf("buffered len = %d, want 3", len(buffered))
	}
	if string(buffered[2].Payload) != "hello" {
		t.Fatalf("last payload = %q, want hello", buffered[2].Payload)
	}
}

func TestReadStreamBootstrapWithTimeout_DisabledAllowsLatePayload(t *testing.T) {
	t.Parallel()

	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("late")}
		close(ch)
	}()

	// timeout <= 0 disables the first-chunk deadline.
	buffered, closed, err := readStreamBootstrapWithTimeout(context.Background(), ch, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed {
		t.Fatal("closed = true, want false")
	}
	if len(buffered) != 1 || string(buffered[0].Payload) != "late" {
		t.Fatalf("buffered = %#v, want late payload", buffered)
	}
}

func TestReadStreamBootstrapWithTimeoutFor_ClosesBeforeContentIsRetryable(t *testing.T) {
	t.Parallel()

	// A Claude stream that emits only protocol metadata (message_start) and then
	// closes without any content must be reported as a retryable failure.
	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-1\",\"model\":\"claude\"}}\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"ping\"}\n")}
	close(ch)

	buffered, closed, err := readStreamBootstrapWithTimeoutFor(context.Background(), ch, time.Second, sdktranslator.FormatClaude)
	if err == nil {
		t.Fatal("expected empty-stream error before content")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		t.Fatalf("error type = %T (%v), want *Error", err, err)
	}
	if authErr.Code != "empty_stream" {
		t.Fatalf("code = %q, want empty_stream", authErr.Code)
	}
	if !authErr.Retryable {
		t.Fatal("expected retryable empty stream")
	}
	if authErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", authErr.HTTPStatus, http.StatusBadGateway)
	}
	if !closed {
		t.Fatal("closed = false, want true")
	}
	if len(buffered) != 0 {
		t.Fatalf("buffered = %#v, want empty", buffered)
	}
}

func TestReadStreamBootstrapWithTimeoutFor_BuffersMetadataUntilContent(t *testing.T) {
	t.Parallel()

	ch := make(chan cliproxyexecutor.StreamChunk, 4)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m-1\",\"model\":\"claude\"}}\n\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"content_block_start\",\"index\":0}\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n")}
	close(ch)

	buffered, closed, err := readStreamBootstrapWithTimeoutFor(context.Background(), ch, time.Second, sdktranslator.FormatClaude)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed {
		t.Fatal("closed = true, want false after content")
	}
	if len(buffered) != 3 {
		t.Fatalf("buffered len = %d, want 3 (metadata + content)", len(buffered))
	}
	if got := string(buffered[2].Payload); !strings.Contains(got, "content_block_delta") {
		t.Fatalf("last payload = %q, want content delta", got)
	}
}

func TestReadStreamBootstrapWithTimeoutFor_ErrorAfterMetadataIsRetryable(t *testing.T) {
	t.Parallel()

	// A Claude stream interrupted after message_start must surface the error so
	// the caller can retry instead of committing a truncated response.
	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-1\",\"model\":\"claude\"}}\n")}
	ch <- cliproxyexecutor.StreamChunk{Err: context.Canceled}
	close(ch)

	_, _, err := readStreamBootstrapWithTimeoutFor(context.Background(), ch, time.Second, sdktranslator.FormatClaude)
	if err == nil {
		t.Fatal("expected error after metadata")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestIsStreamContentPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		want    bool
	}{
		{name: "claude message_start", format: sdktranslator.FormatClaude, payload: `data: {"type":"message_start"}`, want: false},
		{name: "claude content delta", format: sdktranslator.FormatClaude, payload: `data: {"type":"content_block_delta","delta":{"text":"hi"}}`, want: true},
		{name: "claude message_stop", format: sdktranslator.FormatClaude, payload: `data: {"type":"message_stop"}`, want: true},
		{name: "claude event frame", format: sdktranslator.FormatClaude, payload: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", want: false},
		{name: "claude raw passthrough line", format: sdktranslator.FormatClaude, payload: "event: ping\n", want: true},
		{name: "openai role delta", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"role":"assistant"}}]}`, want: false},
		{name: "openai content delta", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"content":"hi"}}]}`, want: true},
		{name: "openai reasoning delta", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`, want: true},
		{name: "openai usage chunk", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[],"usage":{"total_tokens":1}}`, want: false},
		{name: "openai done marker", format: sdktranslator.FormatOpenAI, payload: "data: [DONE]\n", want: true},
		{name: "responses created", format: sdktranslator.FormatOpenAIResponse, payload: `data: {"type":"response.created"}`, want: false},
		{name: "responses text delta", format: sdktranslator.FormatOpenAIResponse, payload: `data: {"type":"response.output_text.delta","delta":"hi"}`, want: true},
		{name: "responses reasoning delta", format: sdktranslator.FormatOpenAIResponse, payload: `data: {"type":"response.reasoning_summary_text.delta","delta":"think"}`, want: true},
		{name: "responses completed", format: sdktranslator.FormatOpenAIResponse, payload: `data: {"type":"response.completed"}`, want: true},
		{name: "gemini candidates empty", format: sdktranslator.FormatGemini, payload: `data: {"candidates":[]}`, want: false},
		{name: "gemini text", format: sdktranslator.FormatGemini, payload: `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, want: true},
		{name: "gemini empty text", format: sdktranslator.FormatGemini, payload: `data: {"candidates":[{"content":{"parts":[{"text":""}]}}]}`, want: false},
		{name: "unknown format first payload", format: sdktranslator.FormatCodex, payload: `{"type":"whatever"}`, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamContentPayload(tc.format, []byte(tc.payload)); got != tc.want {
				t.Fatalf("isStreamContentPayload(%q, %q) = %v, want %v", tc.format, tc.payload, got, tc.want)
			}
		})
	}
}

func TestManagerFirstChunkTimeoutDefault(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, nil, nil)
	if got := m.firstChunkTimeout(); got != 30*time.Second {
		t.Fatalf("default firstChunkTimeout = %v, want 30s", got)
	}

	m.SetConfig(nil) // stores empty config
	if got := m.firstChunkTimeout(); got != 30*time.Second {
		t.Fatalf("empty config firstChunkTimeout = %v, want 30s", got)
	}
}
