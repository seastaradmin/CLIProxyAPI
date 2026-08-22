package executor

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const deepSeekSupportedCustomTool = "apply_patch"

// sanitizeOpenAICompatTools removes custom tools that DeepSeek Responses rejects.
// DeepSeek currently accepts apply_patch as its only custom tool.
func sanitizeOpenAICompatTools(provider string, auth *cliproxyauth.Auth, to sdktranslator.Format, payload []byte) []byte {
	if to.String() != "openai-response" || !isDeepSeekProvider(provider, auth) || !gjson.ValidBytes(payload) {
		return payload
	}

	updated := payload
	filterToolsAtPath := func(path string) {
		tools := gjson.GetBytes(updated, path)
		if !tools.IsArray() {
			return
		}
		filtered := make([]json.RawMessage, 0, len(tools.Array()))
		changed := false
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "custom" &&
				!strings.EqualFold(strings.TrimSpace(tool.Get("name").String()), deepSeekSupportedCustomTool) {
				changed = true
				continue
			}
			filtered = append(filtered, json.RawMessage(tool.Raw))
		}
		if !changed {
			return
		}
		raw, errMarshal := json.Marshal(filtered)
		if errMarshal != nil {
			return
		}
		updated, _ = sjson.SetRawBytes(updated, path, raw)
	}

	filterToolsAtPath("tools")
	input := gjson.GetBytes(updated, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if item.Get("type").String() != "additional_tools" {
				continue
			}
			path := "input." + strconv.Itoa(index) + ".tools"
			tools := gjson.GetBytes(updated, path)
			if !tools.IsArray() {
				continue
			}
			filterToolsAtPath(path)
		}
	}

	// Keep tool_choice consistent with the filtered declarations.
	choice := gjson.GetBytes(updated, "tool_choice")
	if choice.IsObject() && choice.Get("type").String() == "allowed_tools" {
		tools := choice.Get("tools")
		if tools.IsArray() {
			filtered := make([]json.RawMessage, 0, len(tools.Array()))
			for _, tool := range tools.Array() {
				if tool.Get("type").String() == "custom" &&
					!strings.EqualFold(strings.TrimSpace(tool.Get("name").String()), deepSeekSupportedCustomTool) {
					continue
				}
				filtered = append(filtered, json.RawMessage(tool.Raw))
			}
			if len(filtered) == 0 {
				updated, _ = sjson.DeleteBytes(updated, "tool_choice")
			} else if raw, errMarshal := json.Marshal(filtered); errMarshal == nil {
				updated, _ = sjson.SetRawBytes(updated, "tool_choice.tools", raw)
			}
		}
	}
	return updated
}

func isDeepSeekProvider(provider string, auth *cliproxyauth.Auth) bool {
	values := []string{provider}
	if auth != nil {
		if auth.Attributes != nil {
			values = append(values, auth.Attributes["compat_name"], auth.Attributes["base_url"])
		}
		values = append(values, auth.Label, auth.Provider)
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(strings.ToLower(value), "deepseek") {
			return true
		}
		parsed, errParse := url.Parse(value)
		if errParse == nil && strings.Contains(strings.ToLower(parsed.Hostname()), "deepseek") {
			return true
		}
	}
	return false
}
