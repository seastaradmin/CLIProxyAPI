package management

import (
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOpencodeCookieFromAuth(t *testing.T) {
	cases := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{
			name: "cookie with auth prefix",
			auth: &coreauth.Auth{Metadata: map[string]any{"cookie": "auth=Fe26.2**abc"}},
			want: "auth=Fe26.2**abc",
		},
		{
			name: "raw cookie value",
			auth: &coreauth.Auth{Metadata: map[string]any{"cookie": "Fe26.2**abc"}},
			want: "auth=Fe26.2**abc",
		},
		{
			name: "api key",
			auth: &coreauth.Auth{Metadata: map[string]any{"api_key": "sk-123"}},
			want: "",
		},
		{
			name: "empty",
			auth: &coreauth.Auth{Metadata: map[string]any{}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := opencodeCookieFromAuth(tc.auth); got != tc.want {
				t.Fatalf("opencodeCookieFromAuth() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpencodeAPIKeyFromAuth(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata:   map[string]any{"api_key": "sk-abc"},
		Attributes: map[string]string{"api_key": "sk-attr"},
	}
	if got := opencodeAPIKeyFromAuth(auth); got != "sk-abc" {
		t.Fatalf("opencodeAPIKeyFromAuth() = %q, want sk-abc", got)
	}
}

func TestGetOpenCodeUsageDecodesWindow(t *testing.T) {
	payload := []byte(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-08-25T14:17:50.290Z"},"weekly":{"status":"ok","percent":0,"resetsAt":"2026-08-31T00:00:00.290Z"},"monthly":{"status":"rate-limited","percent":100,"resetsAt":"2026-08-27T13:34:17.290Z"}}}`)
	var wrapper struct {
		Usage *opencodeUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		t.Fatalf("failed to decode usage: %v", err)
	}
	if wrapper.Usage.Rolling == nil || wrapper.Usage.Rolling.Percent != 0 {
		t.Fatalf("unexpected rolling window: %+v", wrapper.Usage.Rolling)
	}
	if wrapper.Usage.Monthly == nil || wrapper.Usage.Monthly.Status != "rate-limited" {
		t.Fatalf("unexpected monthly window: %+v", wrapper.Usage.Monthly)
	}
}
