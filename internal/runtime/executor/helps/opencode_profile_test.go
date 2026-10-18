package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenCodeProfileKeepsClientTools(t *testing.T) {
	original := `{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}`
	payload, stream, err := openCodeProfilePayload([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` + original + `],"tool_choice":"required"}`))
	if err != nil {
		t.Fatal(err)
	}
	if stream || !gjson.GetBytes(payload, "stream").Bool() {
		t.Fatal("must force only the upstream stream flag")
	}
	if gjson.GetBytes(payload, "messages.0.content").String() != "hi" {
		t.Fatal("changed user message")
	}
	var got, want any
	_ = json.Unmarshal([]byte(gjson.GetBytes(payload, "tools.0").Raw), &got)
	_ = json.Unmarshal([]byte(original), &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatal("changed client tool schema")
	}
	if gjson.GetBytes(payload, "tool_choice.allowed_tools.mode").String() != "required" || gjson.GetBytes(payload, "tool_choice.allowed_tools.tools.#").Int() != 1 || gjson.GetBytes(payload, "tool_choice.allowed_tools.tools.0.function.name").String() != "get_weather" {
		t.Fatal("profile-only tools must not become callable")
	}
	payload, _, err = openCodeProfilePayload([]byte(`{"messages":[],"stream":true}`))
	if err != nil || gjson.GetBytes(payload, "tool_choice").String() != "none" {
		t.Fatalf("text-only requests must disable tool calls: %v", err)
	}
	for _, body := range []string{`{"tools":[],"tool_choice":"required"}`, `{"tool_choice":{"type":"function","function":{"name":"shell"}}}`, `{"functions":[]}`, `{"stream":"true"}`} {
		if _, _, err := openCodeProfilePayload([]byte(body)); err == nil {
			t.Fatalf("accepted invalid request %s", body)
		}
	}
}

func TestOpenCodeProfileTransport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if !gjson.GetBytes(raw, "stream").Bool() || gjson.GetBytes(raw, "tools.#").Int() != 12 || gjson.GetBytes(raw, "tool_choice").String() != "none" {
			t.Errorf("incorrect upstream payload: %s", raw)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "opencode/") || !strings.HasPrefix(r.Header.Get("x-opencode-session"), "ses_") {
			t.Error("missing profile headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chat-1\",\"model\":\"m\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"O\",\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"K\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	base := upstream.Client()
	if WithClientProfile(base, "") != base {
		t.Fatal("disabled profile must not wrap client")
	}
	client := WithClientProfile(base, "opencode")
	for _, stream := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"m","messages":[],"stream":%t}`, stream)))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if err != nil {
			t.Fatal(err)
		}
		if stream {
			if !strings.Contains(string(raw), "[DONE]") {
				t.Fatal("stream lost")
			}
			continue
		}
		if resp.Header.Get("Content-Type") != "application/json" || gjson.GetBytes(raw, "choices.0.message.content").String() != "OK" || gjson.GetBytes(raw, "usage.total_tokens").Int() != 5 || gjson.GetBytes(raw, "choices.0.message.reasoning_content").String() != "think" {
			t.Fatalf("bad aggregation: %s", raw)
		}
	}
}

func TestCollectOpenCodeChatToolDeltasAndTruncation(t *testing.T) {
	events := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	raw := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
	if _, err := collectOpenCodeChat(strings.NewReader(raw)); err == nil {
		t.Fatal("accepted truncated stream")
	}
	out, err := collectOpenCodeChat(strings.NewReader(raw + "data: [DONE]\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String() != `{"city":"Beijing"}` {
		t.Fatalf("bad tool assembly: %s", out)
	}
	if _, err := collectOpenCodeChat(strings.NewReader("data: {\"error\":{\"message\":\"failure\"}}\n\ndata: [DONE]\n\n")); err == nil {
		t.Fatal("accepted upstream error")
	}
}

// This opt-in test performs real requests only when explicitly enabled locally.
func TestOpenCodeProfileLive(t *testing.T) {
	if os.Getenv("CPA_TEST_OPENCODE_PROFILE") != "1" {
		t.Skip("live upstream test disabled")
	}
	client := WithClientProfile(&http.Client{}, "opencode")
	for _, tc := range []struct{ name, body string }{
		{"text", `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"Reply exactly OK."}],"stream":false}`},
		{"tool", `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"Call get_weather for Beijing. Do not answer with text."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"required","stream":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer public")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if errClose := resp.Body.Close(); errClose != nil {
				t.Fatal(errClose)
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if tc.name == "text" && strings.TrimSpace(gjson.GetBytes(body, "choices.0.message.content").String()) != "OK" {
				t.Fatalf("unexpected text: %s", body)
			}
			if tc.name == "tool" && gjson.GetBytes(body, "choices.0.message.tool_calls.0.function.name").String() != "get_weather" {
				t.Fatalf("client tool was not preserved: %s", body)
			}
			t.Log("upstream returned 200 with expected response")
		})
	}
}
