package controlplane

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestActualUsageTokensPrefersCanonicalReportedTotal(t *testing.T) {
	detail := usage.Detail{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  42,
	}
	if got := actualUsageTokens(detail); got != 42 {
		t.Fatalf("actualUsageTokens() = %d, want 42", got)
	}
}

func TestActualUsageTokensDerivesFromReportedBuckets(t *testing.T) {
	detail := usage.Detail{InputTokens: 10, OutputTokens: 5}
	if got := actualUsageTokens(detail); got != 15 {
		t.Fatalf("actualUsageTokens() = %d, want 15", got)
	}
}
