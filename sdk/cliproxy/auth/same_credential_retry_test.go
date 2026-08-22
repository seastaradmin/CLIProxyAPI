package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// flakyStreamExecutor fails the first failCnt[authID] ExecuteStream calls for a
// credential and succeeds afterwards, recording every call per credential.
type flakyStreamExecutor struct {
	id      string
	mu      sync.Mutex
	failCnt map[string]int
	failErr map[string]error
	calls   map[string]int
}

func (e *flakyStreamExecutor) Identifier() string { return e.id }

func (e *flakyStreamExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *flakyStreamExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	if e.calls == nil {
		e.calls = make(map[string]int)
	}
	e.calls[auth.ID]++
	call := e.calls[auth.ID]
	fail := e.failCnt[auth.ID] >= call
	e.mu.Unlock()

	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if fail {
		err := e.failErr[auth.ID]
		if err == nil {
			err = &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream transient failure"}
		}
		ch <- cliproxyexecutor.StreamChunk{Err: err}
	} else {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *flakyStreamExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *flakyStreamExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *flakyStreamExecutor) HttpRequest(_ context.Context, auth *Auth, _ *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *flakyStreamExecutor) CallCount(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[authID]
}

func newSameCredentialRetryManager(t *testing.T, executor *flakyStreamExecutor, requestRetry int) *Manager {
	t.Helper()
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetRetryConfig(requestRetry, 30*time.Second, 0)
	m.RegisterExecutor(executor)
	registerAuth := func(id string) {
		auth := &Auth{ID: id, Provider: executor.Identifier(), Status: StatusActive, Metadata: map[string]any{"email": id + "@example.com"}}
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	registerAuth("auth-a")
	registerAuth("auth-b")
	return m
}

func drainStream(t *testing.T, result *cliproxyexecutor.StreamResult, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatal("nil stream result")
	}
	var got []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got = append(got, chunk.Payload...)
	}
	return got
}

func TestExecuteStream_RetriesSameCredentialUntilSuccess(t *testing.T) {
	executor := &flakyStreamExecutor{id: "flaky", failCnt: map[string]int{"auth-a": 2}}
	m := newSameCredentialRetryManager(t, executor, 3)

	result, errStream := m.ExecuteStream(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	got := drainStream(t, result, errStream)

	if string(got) != "auth-a" {
		t.Fatalf("payload = %q, want auth-a", got)
	}
	if calls := executor.CallCount("auth-a"); calls != 3 {
		t.Fatalf("auth-a calls = %d, want 3 (2 failures + 1 success on the same credential)", calls)
	}
	if calls := executor.CallCount("auth-b"); calls != 0 {
		t.Fatalf("auth-b calls = %d, want 0 (must not fall back while same-credential retries can still succeed)", calls)
	}
}

func TestExecuteStream_RetriesSameCredentialThenFallsBack(t *testing.T) {
	executor := &flakyStreamExecutor{id: "flaky", failCnt: map[string]int{"auth-a": 100}}
	m := newSameCredentialRetryManager(t, executor, 3)

	result, errStream := m.ExecuteStream(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	got := drainStream(t, result, errStream)

	if string(got) != "auth-b" {
		t.Fatalf("payload = %q, want auth-b (fallback after retry exhaustion)", got)
	}
	if calls := executor.CallCount("auth-a"); calls != 4 {
		t.Fatalf("auth-a calls = %d, want 4 (initial + 3 same-credential retries)", calls)
	}
	if calls := executor.CallCount("auth-b"); calls != 1 {
		t.Fatalf("auth-b calls = %d, want 1", calls)
	}
}

func TestExecuteStream_InvalidRequestRetriesThenRotates(t *testing.T) {
	// A deterministic 400 (invalid_request_error) is retried on the same
	// credential first (upstream 4xx can be transient), and only rotates to the
	// next credential after the retry budget is exhausted.
	executor := &flakyStreamExecutor{
		id:      "flaky",
		failCnt: map[string]int{"auth-a": 100},
		failErr: map[string]error{"auth-a": &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: bad body"}},
	}
	m := newSameCredentialRetryManager(t, executor, 3)

	result, errStream := m.ExecuteStream(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	got := drainStream(t, result, errStream)

	if string(got) != "auth-b" {
		t.Fatalf("payload = %q, want auth-b (fallback after retry exhaustion)", got)
	}
	if calls := executor.CallCount("auth-a"); calls != 4 {
		t.Fatalf("auth-a calls = %d, want 4 (initial + 3 same-credential retries before rotation)", calls)
	}
	if calls := executor.CallCount("auth-b"); calls != 1 {
		t.Fatalf("auth-b calls = %d, want 1", calls)
	}
}

func TestExecuteStream_RateLimitRotatesWithoutSameCredentialRetry(t *testing.T) {
	// Run-limit (429) must rotate to the next credential directly: retrying the
	// same credential would hit the same limit again.
	executor := &flakyStreamExecutor{
		id:      "flaky",
		failCnt: map[string]int{"auth-a": 100},
		failErr: map[string]error{"auth-a": &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit reached"}},
	}
	m := newSameCredentialRetryManager(t, executor, 3)

	result, errStream := m.ExecuteStream(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	got := drainStream(t, result, errStream)

	if string(got) != "auth-b" {
		t.Fatalf("payload = %q, want auth-b (429 must rotate)", got)
	}
	if calls := executor.CallCount("auth-a"); calls != 1 {
		t.Fatalf("auth-a calls = %d, want 1 (no same-credential retry for run-limit)", calls)
	}
	if calls := executor.CallCount("auth-b"); calls != 1 {
		t.Fatalf("auth-b calls = %d, want 1", calls)
	}
}
func TestExecuteStream_NoSameCredentialRetryWhenDisabled(t *testing.T) {
	executor := &flakyStreamExecutor{id: "flaky", failCnt: map[string]int{"auth-a": 100}}
	m := newSameCredentialRetryManager(t, executor, 0)

	result, errStream := m.ExecuteStream(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	got := drainStream(t, result, errStream)

	if string(got) != "auth-b" {
		t.Fatalf("payload = %q, want auth-b", got)
	}
	if calls := executor.CallCount("auth-a"); calls != 1 {
		t.Fatalf("auth-a calls = %d, want 1 (no same-credential retry when request-retry=0)", calls)
	}
	if calls := executor.CallCount("auth-b"); calls != 1 {
		t.Fatalf("auth-b calls = %d, want 1", calls)
	}
}

func TestModelUnavailableClassification(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "shuaiapi wording", message: "Error from provider (Console Go): Upstream request failed: Model is unavailable.", want: true},
		{name: "plain model unavailable", message: "model unavailable", want: true},
		{name: "model not found", message: "model not found", want: true},
		{name: "not supported stays permanent", message: "model not supported", want: false},
		{name: "unsupported stays permanent", message: "requested model is not supported", want: false},
		{name: "plan restriction stays permanent", message: "not available for your plan", want: false},
		{name: "unrelated message", message: "invalid_request_error: bad body", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelUnavailableErrorMessage(tc.message); got != tc.want {
				t.Fatalf("isModelUnavailableErrorMessage(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

func TestIsSameCredentialRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "transient 5xx", err: &Error{HTTPStatus: http.StatusBadGateway, Message: "boom"}, want: true},
		{name: "unexpected EOF", err: &Error{HTTPStatus: http.StatusBadGateway, Message: "unexpected EOF"}, want: true},
		{name: "model unavailable", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "Model is unavailable."}, want: true},
		{name: "invalid request retries", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: bad body"}, want: true},
		{name: "rate limit rotates directly", err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit reached"}, want: false},
		{name: "quota exhausted rotates directly", err: &Error{HTTPStatus: http.StatusBadRequest, Code: "quota_exhausted", Message: "quota"}, want: false},
		{name: "nil error", err: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSameCredentialRetryable(tc.err); got != tc.want {
				t.Fatalf("isSameCredentialRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
