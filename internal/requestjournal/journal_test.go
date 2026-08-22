package requestjournal

import (
	"context"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestFromUsageClassifiesFailuresAndMasksSecrets(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		kind       string
	}{
		{name: "quota", statusCode: 429, body: `{"error":{"message":"usage_limit_reached"}}`, kind: "quota_exhausted"},
		{name: "rate limit", statusCode: 429, body: `{"error":{"message":"too many requests"}}`, kind: "rate_limited"},
		{name: "authentication", statusCode: 401, body: "unauthorized", kind: "authentication"},
		{name: "upstream", statusCode: 503, body: "service unavailable", kind: "upstream_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := fromUsage(context.Background(), coreusage.Record{
				Provider:    "test-provider",
				Model:       "test-model",
				AuthType:    "apikey",
				APIKey:      "sk-test-secret-1234",
				Source:      "sk-test-secret-1234",
				RequestedAt: time.Unix(100, 0),
				Failed:      true,
				Fail: coreusage.Failure{
					StatusCode: test.statusCode,
					Body:       test.body,
				},
			})

			if record.FailureKind != test.kind {
				t.Fatalf("failure kind = %q, want %q", record.FailureKind, test.kind)
			}
			if strings.Contains(record.APIKey, "sk-test-secret-1234") || strings.Contains(record.Source, "sk-test-secret-1234") {
				t.Fatalf("record leaked an API key: %+v", record)
			}
		})
	}
}

func TestSanitizeRecordAddsLegacyFailureKind(t *testing.T) {
	record := &Record{
		ID:         "legacy",
		AuthType:   "apikey",
		APIKey:     "sk-legacy-secret-1234",
		Source:     "sk-legacy-secret-1234",
		Failed:     true,
		StatusCode: 429,
		ErrorBody:  "quota exhausted",
	}

	sanitizeRecord(record)

	if record.FailureKind != "quota_exhausted" {
		t.Fatalf("failure kind = %q, want quota_exhausted", record.FailureKind)
	}
	if strings.Contains(record.APIKey, "sk-legacy-secret-1234") || strings.Contains(record.Source, "sk-legacy-secret-1234") {
		t.Fatalf("legacy record leaked an API key: %+v", record)
	}
}
