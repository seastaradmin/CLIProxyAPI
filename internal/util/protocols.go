package util

import "strings"

// NormalizeUpstreamProtocols sanitizes protocol labels.
// Supported: chat, responses, messages. Empty input defaults to [chat].
func NormalizeUpstreamProtocols(values []string) []string {
	if len(values) == 0 {
		return []string{"chat"}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		p := strings.ToLower(strings.TrimSpace(raw))
		switch p {
		case "chat", "chat.completions", "chat-completions", "openai":
			p = "chat"
		case "responses", "openai-response", "openai-responses", "response":
			p = "responses"
		case "messages", "claude", "anthropic", "message":
			p = "messages"
		default:
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{"chat"}
	}
	return out
}

// ProtocolsCSV joins normalized protocols with commas for auth attributes.
func ProtocolsCSV(values []string) string {
	return strings.Join(NormalizeUpstreamProtocols(values), ",")
}

// ParseProtocolsCSV parses a comma-separated protocol list.
func ParseProtocolsCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{"chat"}
	}
	parts := strings.Split(raw, ",")
	return NormalizeUpstreamProtocols(parts)
}

// HasProtocol reports whether protocols contains the given label.
func HasProtocol(protocols []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	switch want {
	case "chat", "chat.completions", "chat-completions", "openai":
		want = "chat"
	case "responses", "openai-response", "openai-responses", "response":
		want = "responses"
	case "messages", "claude", "anthropic", "message":
		want = "messages"
	}
	for _, p := range NormalizeUpstreamProtocols(protocols) {
		if p == want {
			return true
		}
	}
	return false
}

// ClientFormatToProtocol maps client SourceFormat to a protocol label.
func ClientFormatToProtocol(sourceFormat string) string {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "openai-response", "openai-responses":
		return "responses"
	case "claude":
		return "messages"
	case "openai", "openai-image", "openai-video":
		return "chat"
	default:
		return ""
	}
}
