package auth

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestIsQuotaLikeError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		code       string
		message    string
		want       bool
	}{
		{name: "ordinary 429", statusCode: http.StatusTooManyRequests, message: "rate limited", want: false},
		{name: "429 quota payload", statusCode: http.StatusTooManyRequests, message: "quota exhausted", want: true},
		{name: "429 quota marker", statusCode: http.StatusTooManyRequests, message: "quota", want: true},
		{name: "usage limit on 403", statusCode: http.StatusForbidden, message: "usage limit reached", want: true},
		{name: "structured usage code", statusCode: http.StatusForbidden, code: "usage_limit_reached", want: true},
		{name: "invalid credentials", statusCode: http.StatusUnauthorized, message: "invalid or expired credentials", want: false},
		{name: "generic forbidden", statusCode: http.StatusForbidden, message: "forbidden", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsQuotaLikeError(tt.statusCode, tt.code, tt.message); got != tt.want {
				t.Fatalf("IsQuotaLikeError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyUsageError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		code       string
		message    string
		want       UsageErrorKind
	}{
		{name: "ordinary 429", statusCode: http.StatusTooManyRequests, message: "too many requests", want: UsageErrorKindRateLimited},
		{name: "rate code", statusCode: http.StatusBadGateway, code: "rate_limit_exceeded", want: UsageErrorKindRateLimited},
		{name: "usage limit", statusCode: http.StatusForbidden, message: "usage limit reached", want: UsageErrorKindQuotaExhausted},
		{name: "resource exhausted", statusCode: http.StatusTooManyRequests, code: "resource_exhausted", want: UsageErrorKindQuotaExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyUsageError(tt.statusCode, tt.code, tt.message); got != tt.want {
				t.Fatalf("ClassifyUsageError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManagerMarkResultUsageLimit403UsesCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	manager := NewManager(nil, nil, nil)
	authRecord := &Auth{ID: "usage-limit-auth", Provider: "codex"}
	if _, errRegister := manager.Register(context.Background(), authRecord); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID: authRecord.ID,
		Model:  "gpt-5",
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    "usage limit reached",
		},
	})

	updated, ok := manager.GetByID(authRecord.ID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("usage limit must not permanently disable auth: disabled=%v status=%q", updated.Disabled, updated.Status)
	}
	state := updated.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded || state.StatusMessage != "quota exhausted" {
		t.Fatalf("model quota state = %+v, want exhausted cooldown", state)
	}
	if state.Quota.Kind != string(UsageErrorKindQuotaExhausted) {
		t.Fatalf("model quota kind = %q, want %q", state.Quota.Kind, UsageErrorKindQuotaExhausted)
	}
	if !state.NextRetryAfter.After(time.Now()) || !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("usage limit cooldown not scheduled: model=%v auth=%v", state.NextRetryAfter, updated.NextRetryAfter)
	}
}

func TestManagerExecuteRotatesAfterUsageErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantKind    UsageErrorKind
		wantMessage string
	}{
		{
			name:        "quota exhaustion",
			err:         &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit reached"},
			wantKind:    UsageErrorKindQuotaExhausted,
			wantMessage: "quota exhausted",
		},
		{
			name:        "ordinary rate limit",
			err:         &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			wantKind:    UsageErrorKindRateLimited,
			wantMessage: "rate limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			badID := "usage-rotation-bad-" + strings.ReplaceAll(tt.name, " ", "-")
			goodID := badID + "-good"
			executor := &authFallbackExecutor{
				id:            "claude",
				executeErrors: map[string]error{badID: tt.err},
			}
			manager.RegisterExecutor(executor)

			badAuth := &Auth{ID: badID, Provider: "claude"}
			goodAuth := &Auth{ID: goodID, Provider: "claude"}
			model := "usage-rotation-model-" + strings.ReplaceAll(tt.name, " ", "-")
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(badID, "claude", []*registry.ModelInfo{{ID: model}})
			reg.RegisterClient(goodID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				reg.UnregisterClient(badID)
				reg.UnregisterClient(goodID)
			})
			if _, errRegister := manager.Register(context.Background(), badAuth); errRegister != nil {
				t.Fatalf("register bad auth: %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), goodAuth); errRegister != nil {
				t.Fatalf("register good auth: %v", errRegister)
			}

			request := cliproxyexecutor.Request{Model: model}
			response, errExecute := manager.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatalf("first execute error = %v", errExecute)
			}
			if string(response.Payload) != goodID {
				t.Fatalf("first response auth = %q, want %q", string(response.Payload), goodID)
			}

			updated, ok := manager.GetByID(badID)
			if !ok || updated == nil {
				t.Fatal("expected failed auth to remain registered")
			}
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable || state.StatusMessage != tt.wantMessage {
				t.Fatalf("failed model state = %+v, want unavailable %q", state, tt.wantMessage)
			}
			if state.Quota.Kind != string(tt.wantKind) || !state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("failed model cooldown = %+v, want kind %q and future retry", state.Quota, tt.wantKind)
			}

			response, errExecute = manager.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
			if errExecute != nil || string(response.Payload) != goodID {
				t.Fatalf("second execute = payload %q error %v, want good auth", string(response.Payload), errExecute)
			}
			calls := executor.ExecuteCalls()
			wantCalls := []string{badID, goodID, goodID}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
			}
		})
	}
}
