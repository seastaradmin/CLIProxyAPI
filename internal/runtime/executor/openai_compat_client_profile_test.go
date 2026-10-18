package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	execution "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatClientProfileTextAndStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "tool_choice").String() != "none" {
			t.Errorf("missing wire profile: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chat-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "profile-test", BaseURL: server.URL, ClientProfile: "opencode"}}}
	e := NewOpenAICompatExecutor("profile-test", cfg)
	a := &auth.Auth{Provider: "profile-test", Attributes: map[string]string{"compat_name": "profile-test", "base_url": server.URL, "api_key": "public"}}
	req := execution.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}
	opts := execution.Options{SourceFormat: translator.FromString("openai")}
	resp, err := e.Execute(context.Background(), a, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(resp.Payload, "choices.0.message.content").String() != "OK" {
		t.Fatalf("bad response %s", resp.Payload)
	}
	opts.Stream = true
	result, err := e.ExecuteStream(context.Background(), a, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		out.Write(chunk.Payload)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("bad stream %s", out.String())
	}
}
