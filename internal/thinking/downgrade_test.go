package thinking

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestHasClaudeAdaptiveThinking(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "adaptive with effort", body: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, want: true},
		{name: "enabled", body: `{"thinking":{"type":"enabled","budget_tokens":4096}}`, want: false},
		{name: "disabled", body: `{"thinking":{"type":"disabled"}}`, want: false},
		{name: "auto", body: `{"thinking":{"type":"auto"}}`, want: false},
		{name: "no thinking", body: `{"model":"claude"}`, want: false},
		{name: "invalid json", body: `{not json`, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasClaudeAdaptiveThinking([]byte(tc.body)); got != tc.want {
				t.Fatalf("HasClaudeAdaptiveThinking(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestDowngradeClaudeAdaptiveThinking(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantType string
		wantKey  string
		wantVal  any
	}{
		{
			name:     "high effort maps to enabled with budget",
			body:     `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"max_tokens":32000}`,
			wantType: "enabled",
			wantKey:  "thinking.budget_tokens",
			wantVal:  int64(24576),
		},
		{
			name:     "unknown effort falls back to auto",
			body:     `{"thinking":{"type":"adaptive"},"output_config":{"effort":"turbo"}}`,
			wantType: "auto",
		},
		{
			name:     "no effort falls back to auto",
			body:     `{"thinking":{"type":"adaptive"}}`,
			wantType: "auto",
		},
		{
			name:     "non-adaptive body unchanged",
			body:     `{"thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantType: "enabled",
			wantKey:  "thinking.budget_tokens",
			wantVal:  int64(4096),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DowngradeClaudeAdaptiveThinking([]byte(tc.body))
			if gotType := gjson.GetBytes(got, "thinking.type").String(); gotType != tc.wantType {
				t.Fatalf("thinking.type = %q, want %q (body=%s)", gotType, tc.wantType, got)
			}
			if tc.wantKey != "" {
				if gotVal := gjson.GetBytes(got, tc.wantKey).Int(); gotVal != tc.wantVal {
					t.Fatalf("%s = %v, want %v (body=%s)", tc.wantKey, gotVal, tc.wantVal, got)
				}
			}
			if gjson.GetBytes(got, "output_config.effort").Exists() {
				t.Fatalf("output_config.effort must be removed (body=%s)", got)
			}
		})
	}
}
