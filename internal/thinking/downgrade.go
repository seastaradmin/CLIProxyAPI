package thinking

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// HasClaudeAdaptiveThinking reports whether the body uses the Claude 4.6
// adaptive thinking form (thinking.type="adaptive" with output_config.effort).
func HasClaudeAdaptiveThinking(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	return gjson.GetBytes(body, "thinking.type").String() == "adaptive"
}

// DowngradeClaudeAdaptiveThinking converts a Claude adaptive thinking request
// into the legacy enabled form for upstreams that only accept
// thinking.type in {enabled, disabled, auto} (common on Anthropic-compatible
// middleboxes). The effort level is mapped to an equivalent budget_tokens; when
// no budget mapping exists, thinking.type falls back to "auto". The original
// body is returned unchanged when there is nothing to downgrade.
func DowngradeClaudeAdaptiveThinking(body []byte) []byte {
	if !HasClaudeAdaptiveThinking(body) {
		return body
	}
	result := body
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if effort != "" {
		if budget, ok := ConvertLevelToBudget(effort); ok {
			result, _ = sjson.SetBytes(result, "thinking.type", "enabled")
			result, _ = sjson.SetBytes(result, "thinking.budget_tokens", budget)
		} else {
			result, _ = sjson.SetBytes(result, "thinking.type", "auto")
			result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		}
	} else {
		result, _ = sjson.SetBytes(result, "thinking.type", "auto")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
	}
	result, _ = sjson.DeleteBytes(result, "output_config.effort")
	if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
		result, _ = sjson.DeleteBytes(result, "output_config")
	}
	return result
}
