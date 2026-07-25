package management

import (
	"context"
	"net/http"
	"testing"
)

func TestNormalizeHealthProtocolsDefaultsToThreeProtocols(t *testing.T) {
	specs, errProtocols := normalizeHealthProtocols(nil)
	if errProtocols != nil {
		t.Fatalf("normalizeHealthProtocols() error = %v", errProtocols)
	}
	if len(specs) != 3 {
		t.Fatalf("len(specs) = %d, want 3", len(specs))
	}
	want := []string{"responses", "chat", "messages"}
	for i := range want {
		if specs[i].name != want[i] {
			t.Fatalf("specs[%d].name = %q, want %q", i, specs[i].name, want[i])
		}
	}
}

func TestSummarizeHealthCheckStatus(t *testing.T) {
	tests := []struct {
		name    string
		results []authFileHealthProtocolResult
		want    string
	}{
		{
			name: "healthy",
			results: []authFileHealthProtocolResult{
				{Status: "success"}, {Status: "success"}, {Status: "success"},
			},
			want: "healthy",
		},
		{
			name: "partial",
			results: []authFileHealthProtocolResult{
				{Status: "success"}, {Status: "error"}, {Status: "error"},
			},
			want: "partial",
		},
		{
			name: "error",
			results: []authFileHealthProtocolResult{
				{Status: "error"}, {Status: "error"}, {Status: "error"},
			},
			want: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeHealthCheckStatus(tt.results); got != tt.want {
				t.Fatalf("summarizeHealthCheckStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractHealthReply(t *testing.T) {
	tests := []struct {
		protocol string
		body     string
		want     string
	}{
		{"responses", `{"output":[{"content":[{"type":"output_text","text":"OK"}]}]}`, "OK"},
		{"chat", `{"choices":[{"message":{"content":"OK"}}]}`, "OK"},
		{"messages", `{"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"OK"}]}`, "OK"},
	}
	for _, tt := range tests {
		if got := extractHealthReply(tt.protocol, []byte(tt.body)); got != tt.want {
			t.Fatalf("extractHealthReply(%q) = %q, want %q", tt.protocol, got, tt.want)
		}
	}
}

func TestClassifyHealthErrorDistinguishesCredentialAndProtocolFailures(t *testing.T) {
	if got := classifyHealthError(context.Background(), http.StatusForbidden, "forbidden"); got != "credential" {
		t.Fatalf("credential classification = %q", got)
	}
	if got := classifyHealthError(context.Background(), http.StatusBadRequest, "unsupported tool"); got != "protocol" {
		t.Fatalf("protocol classification = %q", got)
	}
}
