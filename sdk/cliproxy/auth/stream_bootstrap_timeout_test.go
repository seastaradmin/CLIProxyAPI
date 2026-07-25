package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
