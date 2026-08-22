package auth

import (
	"net/http"
	"strings"
)

// UsageErrorKind identifies the provider-side reason for an automatic cooldown.
type UsageErrorKind string

const (
	UsageErrorKindQuotaExhausted UsageErrorKind = "quota_exhausted"
	UsageErrorKindRateLimited    UsageErrorKind = "rate_limited"
)

// ClassifyUsageError distinguishes exhausted quota from a temporary rate limit.
// Explicit quota markers take precedence over HTTP 429 because providers often
// return quota exhaustion with the same status as ordinary throttling.
func ClassifyUsageError(statusCode int, code, message string) UsageErrorKind {
	text := strings.ToLower(strings.TrimSpace(code + " " + message))
	if containsAny(text, []string{
		"usage_limit_reached",
		"usage limit",
		"usage-limit",
		"quota",
		"quota_exhausted",
		"quota exhausted",
		"quota-exceeded",
		"quota exceeded",
		"insufficient_quota",
		"insufficient quota",
		"resource_exhausted",
		"resource exhausted",
		"free-usage-exhausted",
		"free usage exhausted",
		"insufficient_g1_credits_balance",
		"credit balance exhausted",
		"credits exhausted",
		"out of credits",
	}) {
		return UsageErrorKindQuotaExhausted
	}
	if containsAny(text, []string{
		"rate_limit",
		"rate limit",
		"rate-limit",
		"rate_limited",
		"rate limited",
		"too many requests",
		"throttled",
		"throttle",
	}) || statusCode == http.StatusTooManyRequests {
		return UsageErrorKindRateLimited
	}
	return ""
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsQuotaLikeError reports whether an upstream failure indicates exhausted usage
// or quota. Ordinary HTTP 429 responses are rate limits unless their payload
// contains an explicit quota marker.
func IsQuotaLikeError(statusCode int, code, message string) bool {
	return ClassifyUsageError(statusCode, code, message) == UsageErrorKindQuotaExhausted
}

// IsRateLimitedError reports whether an upstream failure indicates temporary throttling.
func IsRateLimitedError(statusCode int, code, message string) bool {
	return ClassifyUsageError(statusCode, code, message) == UsageErrorKindRateLimited
}
