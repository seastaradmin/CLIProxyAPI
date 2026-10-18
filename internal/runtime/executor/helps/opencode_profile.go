package helps

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// WithClientProfile returns a copy of client with an opt-in upstream wire profile.
// The OpenCode profile supports Chat Completions, including non-stream callers.
func WithClientProfile(client *http.Client, profile string) *http.Client {
	profile = strings.ToLower(strings.TrimSpace(profile))
	if profile == "" || profile == "none" {
		return client
	}
	clone := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &openCodeProfileTransport{base: base, profile: profile}
	return &clone
}

type openCodeProfileTransport struct {
	base    http.RoundTripper
	profile string
}

var openCodeToolNames = []string{"edit", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "execute"}

func (t *openCodeProfileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.profile != "opencode" {
		return nil, fmt.Errorf("unsupported client-profile %q", t.profile)
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return nil, fmt.Errorf("opencode client-profile requires a Chat Completions upstream")
	}
	if req.Body == nil {
		return nil, fmt.Errorf("opencode client-profile requires a JSON request body")
	}
	raw, err := io.ReadAll(req.Body)
	errClose := req.Body.Close()
	if err != nil {
		return nil, err
	}
	if errClose != nil {
		return nil, errClose
	}
	payload, stream, err := openCodeProfilePayload(raw)
	if err != nil {
		return nil, err
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(payload))
	out.ContentLength = int64(len(payload))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Accept", "text/event-stream")
	out.Header.Del("Content-Length")
	out.Header.Set("User-Agent", "opencode/2.0.12")
	out.Header.Set("x-opencode-client", "cli")
	seed := util.SessionIDFromContext(req.Context())
	if seed == "" {
		seed = uuid.NewString()
	}
	hash := sha256.Sum256([]byte(seed))
	if out.Header.Get("x-opencode-session") == "" {
		out.Header.Set("x-opencode-session", "ses_"+hex.EncodeToString(hash[:])[:26])
	}
	out.Header.Set("x-opencode-request", "msg_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:26])
	if out.Header.Get("x-opencode-project") == "" {
		out.Header.Set("x-opencode-project", "global")
	}
	resp, err := t.base.RoundTrip(out)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || stream {
		return resp, err
	}
	// The upstream requires streaming even when the downstream requested JSON.
	data, err := collectOpenCodeChat(resp.Body)
	errClose = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if errClose != nil {
		return nil, errClose
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.TransferEncoding = nil
	return resp, nil
}

func openCodeProfilePayload(raw []byte) ([]byte, bool, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, false, fmt.Errorf("opencode client-profile requires a JSON object")
	}
	var stream bool
	if value := body["stream"]; len(value) != 0 {
		if err := json.Unmarshal(value, &stream); err != nil {
			return nil, false, fmt.Errorf("opencode client-profile: invalid stream flag")
		}
	}
	if len(body["functions"]) != 0 || len(body["function_call"]) != 0 {
		return nil, false, fmt.Errorf("opencode client-profile requires tools instead of legacy functions")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(bodyOr(body["tools"], "[]"), &tools); err != nil {
		return nil, false, fmt.Errorf("opencode client-profile: invalid tools: %w", err)
	}
	names := make(map[string]bool)
	allowed := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(rawTool, &tool); err != nil || tool.Type != "function" || tool.Function.Name == "" {
			return nil, false, fmt.Errorf("opencode client-profile only supports named function tools")
		}
		names[tool.Function.Name] = true
		allowed = append(allowed, map[string]any{"type": "function", "function": map[string]string{"name": tool.Function.Name}})
	}
	for _, name := range openCodeToolNames {
		if names[name] {
			continue
		}
		tool, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": "Not available in this request.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}})
		tools = append(tools, tool)
	}
	choice := strings.TrimSpace(string(body["tool_choice"]))
	switch choice {
	case "", "null", `"auto"`, `"required"`:
		if len(allowed) == 0 {
			if choice == `"required"` {
				return nil, false, fmt.Errorf("opencode client-profile: required tool choice needs client tools")
			}
			body["tool_choice"] = json.RawMessage(`"none"`)
		} else {
			mode := "auto"
			if choice == `"required"` {
				mode = "required"
			}
			body["tool_choice"], _ = json.Marshal(map[string]any{"type": "allowed_tools", "allowed_tools": map[string]any{"mode": mode, "tools": allowed}})
		}
	case `"none"`:
	default:
		// Explicit choices must never gain access to profile-only tools.
		var c struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			AllowedTools struct {
				Mode  string `json:"mode"`
				Tools []struct {
					Type     string `json:"type"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			} `json:"allowed_tools"`
		}
		if err := json.Unmarshal(body["tool_choice"], &c); err != nil {
			return nil, false, fmt.Errorf("opencode client-profile: invalid tool_choice")
		}
		valid := c.Type == "function" && names[c.Function.Name]
		if c.Type == "allowed_tools" {
			valid = len(c.AllowedTools.Tools) > 0 && (c.AllowedTools.Mode == "auto" || c.AllowedTools.Mode == "required")
			for _, tool := range c.AllowedTools.Tools {
				valid = valid && tool.Type == "function" && names[tool.Function.Name]
			}
		}
		if !valid {
			return nil, false, fmt.Errorf("opencode client-profile requires a choice naming an original client tool")
		}
	}
	body["tools"], _ = json.Marshal(tools)
	body["stream"] = json.RawMessage("true")
	var options map[string]any
	if value := body["stream_options"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &options); err != nil {
			return nil, false, fmt.Errorf("opencode client-profile: invalid stream_options")
		}
	}
	if options == nil {
		options = make(map[string]any)
	}
	options["include_usage"] = true
	body["stream_options"], _ = json.Marshal(options)
	payload, err := json.Marshal(body)
	return payload, stream, err
}

func bodyOr(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}

// collectOpenCodeChat rejects truncated streams instead of returning partial success.
func collectOpenCodeChat(reader io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	result := map[string]any{"object": "chat.completion"}
	choices := make(map[int]map[string]any)
	toolCalls := make(map[int]map[int]map[string]any)
	done := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var event struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Created json.RawMessage `json:"created"`
			Usage   json.RawMessage `json:"usage"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index        int                        `json:"index"`
				Delta        map[string]json.RawMessage `json:"delta"`
				FinishReason any                        `json:"finish_reason"`
				Logprobs     any                        `json:"logprobs"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, fmt.Errorf("opencode stream: invalid event: %w", err)
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			return nil, fmt.Errorf("opencode stream: %s", event.Error)
		}
		if event.ID != "" {
			result["id"] = event.ID
		}
		if event.Model != "" {
			result["model"] = event.Model
		}
		if len(event.Created) > 0 {
			result["created"] = event.Created
		}
		if len(event.Usage) > 0 && string(event.Usage) != "null" {
			result["usage"] = event.Usage
		}
		for _, c := range event.Choices {
			choice := choices[c.Index]
			if choice == nil {
				choice = map[string]any{"index": c.Index, "message": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}
				choices[c.Index] = choice
			}
			message := choice["message"].(map[string]any)
			for key, value := range c.Delta {
				switch key {
				case "content", "reasoning_content", "reasoning", "refusal":
					var chunk string
					if err := json.Unmarshal(value, &chunk); err != nil {
						return nil, fmt.Errorf("opencode stream: invalid %s delta", key)
					}
					if string(value) != "null" {
						previous, _ := message[key].(string)
						message[key] = previous + chunk
					}
				case "tool_calls":
					var calls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					}
					if err := json.Unmarshal(value, &calls); err != nil {
						return nil, fmt.Errorf("opencode stream: invalid tool delta")
					}
					if toolCalls[c.Index] == nil {
						toolCalls[c.Index] = make(map[int]map[string]any)
					}
					for _, call := range calls {
						target := toolCalls[c.Index][call.Index]
						if target == nil {
							target = map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}}
							toolCalls[c.Index][call.Index] = target
						}
						if call.ID != "" {
							target["id"] = call.ID
						}
						fn := target["function"].(map[string]any)
						fn["name"] = fn["name"].(string) + call.Function.Name
						fn["arguments"] = fn["arguments"].(string) + call.Function.Arguments
					}
				default:
					message[key] = value
				}
			}
			if c.FinishReason != nil {
				choice["finish_reason"] = c.FinishReason
			}
			if c.Logprobs != nil {
				return nil, fmt.Errorf("opencode client-profile cannot aggregate logprobs; use stream=true")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("opencode stream: %w", err)
	}
	if !done || len(choices) == 0 {
		return nil, fmt.Errorf("opencode stream ended without a complete response")
	}
	indexes := make([]int, 0, len(choices))
	for index := range choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	list := make([]any, 0, len(indexes))
	for _, index := range indexes {
		choice := choices[index]
		if choice["finish_reason"] == nil {
			return nil, fmt.Errorf("opencode stream ended without finish_reason")
		}
		if calls := toolCalls[index]; len(calls) > 0 {
			callIndexes := make([]int, 0, len(calls))
			for i := range calls {
				callIndexes = append(callIndexes, i)
			}
			sort.Ints(callIndexes)
			ordered := make([]any, 0, len(calls))
			for _, i := range callIndexes {
				ordered = append(ordered, calls[i])
			}
			choice["message"].(map[string]any)["tool_calls"] = ordered
		}
		list = append(list, choice)
	}
	result["choices"] = list
	return json.Marshal(result)
}
