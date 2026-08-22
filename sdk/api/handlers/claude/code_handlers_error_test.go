package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := handlers.PendingStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}

func TestClaudeErrorNilTypeFallsBackToStatusType(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "nil placeholder type", message: `{"error":{"type":"<nil>","message":"Upstream request failed: Model is unavailable."},"type":"error"}`, want: "invalid_request_error"},
		{name: "null type value", message: `{"error":{"type":null,"message":"boom"}}`, want: "invalid_request_error"},
		{name: "real error type preserved", message: `{"error":{"type":"rate_limit_error","message":"slow down"}}`, want: "rate_limit_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(tc.message)}
			got := handler.toClaudeError(msg)
			if got.Error.Type != tc.want {
				t.Fatalf("error.type = %q, want %q", got.Error.Type, tc.want)
			}
		})
	}
}

func TestIsNilErrorType(t *testing.T) {
	for _, v := range []string{"<nil>", "nil", "null", "none", "unknown", "", " <nil> "} {
		if !isNilErrorType(v) {
			t.Fatalf("isNilErrorType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"invalid_request_error", "rate_limit_error", "overloaded_error", "api_error"} {
		if isNilErrorType(v) {
			t.Fatalf("isNilErrorType(%q) = true, want false", v)
		}
	}
}

func TestIsRetryableStreamInterruption(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: true},
		{name: "plain EOF", err: io.EOF, want: true},
		{name: "net error", err: &net.OpError{Op: "read", Err: errors.New("connection reset")}, want: true},
		{name: "wrapped context canceled", err: fmt.Errorf("upstream read: %w", context.Canceled), want: true},
		{name: "plain error", err: errors.New("boom"), want: false},
		{name: "nil error", err: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableStreamInterruption(tc.err); got != tc.want {
				t.Fatalf("isRetryableStreamInterruption(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMaybeMarkStreamInterruptionRetryable(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}

	t.Run("transport interruption maps to 529", func(t *testing.T) {
		msg := &interfaces.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      context.Canceled,
		}
		handler.maybeMarkStreamInterruptionRetryable(msg)
		if msg.StatusCode != 529 {
			t.Fatalf("status = %d, want 529", msg.StatusCode)
		}
	})

	t.Run("real upstream status is preserved", func(t *testing.T) {
		msg := &interfaces.ErrorMessage{
			StatusCode: http.StatusUnauthorized,
			Error:      errors.New("unauthorized"),
		}
		handler.maybeMarkStreamInterruptionRetryable(msg)
		if msg.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", msg.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("overloaded error envelope", func(t *testing.T) {
		msg := &interfaces.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      io.ErrUnexpectedEOF,
		}
		handler.maybeMarkStreamInterruptionRetryable(msg)
		got := handler.toClaudeError(msg)
		if got.Error.Type != "overloaded_error" {
			t.Fatalf("error.type = %q, want overloaded_error", got.Error.Type)
		}
	})
}
