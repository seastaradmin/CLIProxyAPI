package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestExecute404FallbackTogglesV1AndRemembers verifies the endpoint self-healing:
// a 404 on the default path triggers one retry on the /v1 alternate, the working
// suffix is remembered in-process, and follow-up requests take it directly.
func TestExecute404FallbackTogglesV1AndRemembers(t *testing.T) {
	var hits404, hits200, followUps int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			atomic.AddInt32(&hits404, 1)
			w.WriteHeader(http.StatusNotFound)
		case "/v1/chat/completions":
			if atomic.AddInt32(&hits200, 1) > 1 {
				atomic.AddInt32(&followUps, 1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	e := NewOpenAICompatExecutor("openai-compatible-selfheal", &config.Config{})
	auth := &cliproxyauth.Auth{
		Label: "selfheal",
		Attributes: map[string]string{
			"compat_name": "selfheal-" + t.Name(),
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	req := cliproxyexecutor.Request{Model: "demo", Payload: []byte(`{"model":"demo","messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}

	resp, err := e.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v, want fallback success", err)
	}
	if !strings.Contains(string(resp.Payload), "chatcmpl_1") {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	if got := atomic.LoadInt32(&hits404); got != 1 {
		t.Fatalf("default path hits = %d, want 1", got)
	}

	// The working suffix must be remembered: the second request goes straight
	// to /v1/chat/completions without touching the 404 path again.
	if _, errSecond := e.Execute(context.Background(), auth, req, opts); errSecond != nil {
		t.Fatalf("second Execute() error = %v", errSecond)
	}
	if got := atomic.LoadInt32(&hits404); got != 1 {
		t.Fatalf("default path hits after follow-up = %d, want still 1 (override not applied)", got)
	}
	if got := atomic.LoadInt32(&followUps); got != 1 {
		t.Fatalf("follow-up hits on /v1 path = %d, want 1", got)
	}
}

// TestExecute404FallbackKeepsOriginalErrorWhenAlternateFails verifies that when
// both path variants fail, the original 404 is reported.
func TestExecute404FallbackKeepsOriginalErrorWhenAlternateFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer server.Close()

	e := NewOpenAICompatExecutor("openai-compatible-selfheal", &config.Config{})
	auth := &cliproxyauth.Auth{
		Label: "selfheal-fail",
		Attributes: map[string]string{
			"compat_name": "selfheal-" + t.Name(),
			"base_url":    server.URL,
			"api_key":     "test",
		},
	}
	req := cliproxyexecutor.Request{Model: "demo", Payload: []byte(`{"model":"demo","messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}

	_, err := e.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want 404")
	}
	type statusCoder interface{ StatusCode() int }
	sc, ok := err.(statusCoder)
	if !ok || sc.StatusCode() != http.StatusNotFound {
		t.Fatalf("error = %v, want status 404", err)
	}
}

func TestToggleV1EndpointSuffix(t *testing.T) {
	cases := map[string]string{
		"/messages":            "/v1/messages",
		"/v1/messages":         "/messages",
		"/chat/completions":    "/v1/chat/completions",
		"/v1/chat/completions": "/chat/completions",
		"":                     "",
		"no-slash":             "",
	}
	for in, want := range cases {
		if got := helps.ToggleV1EndpointSuffix(in); got != want {
			t.Errorf("ToggleV1EndpointSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
