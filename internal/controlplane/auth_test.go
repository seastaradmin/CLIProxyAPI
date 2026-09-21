package controlplane

import (
	"net/http"
	"testing"
)

func TestRequestAuthKeysAcceptsNativeHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bearer-key")
	req.Header.Set("X-Api-Key", "anthropic-key")
	req.Header.Set("X-Goog-Api-Key", "google-key")

	got := requestAuthKeys(req)
	want := []string{"bearer-key", "anthropic-key", "google-key"}
	if len(got) != len(want) {
		t.Fatalf("requestAuthKeys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("requestAuthKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRequestAuthKeysIgnoresNonBearerAuthorization(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic credentials")

	if got := requestAuthKeys(req); len(got) != 0 {
		t.Fatalf("requestAuthKeys() = %v, want no control-plane candidates", got)
	}
}

func TestUnknownControlPlaneKeyFailOpen(t *testing.T) {
	if !unknownControlPlaneKey("KEY_NOT_FOUND") {
		t.Fatal("KEY_NOT_FOUND must fail open to CPA static api-keys")
	}
	if !unknownControlPlaneKey("key_not_found") {
		t.Fatal("lowercase KEY_NOT_FOUND must fail open")
	}
	if !unknownControlPlaneKey("") {
		t.Fatal("empty code with Valid=false is not a known-dead CP key")
	}
	if unknownControlPlaneKey("KEY_REVOKED") {
		t.Fatal("revoked control-plane keys must still 401")
	}
	if unknownControlPlaneKey("key_revoked") {
		t.Fatal("revoked control-plane keys must still 401")
	}
}

func TestTPMUsageKeyIsNamespaced(t *testing.T) {
	if got := tpmUsageKey("abc123"); got != "cp:tpm:usage:abc123" {
		t.Fatalf("tpmUsageKey() = %q", got)
	}
	if got := tpmUsageKey(""); got != "" {
		t.Fatalf("tpmUsageKey(empty) = %q, want empty", got)
	}
}

// IP 访问控制判定：白名单各层交集（空层不限）+ 黑名单并集 + CIDR/精确匹配。
func TestIPDeniedIntersectionAndUnion(t *testing.T) {
	// 黑名单命中 → 拒（即使白名单允许）。
	if denied, reason := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8", IPBlacklistRule: "10.0.0.5"}, "10.0.0.5"); !denied || reason != "ip_blacklisted" {
		t.Fatalf("blacklist should win: %v %s", denied, reason)
	}
	// 白名单命中但不在黑名单 → 放行。
	if denied, _ := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8", IPBlacklistRule: "10.0.0.5"}, "10.0.1.9"); denied {
		t.Fatal("ip inside whitelist and outside blacklist must pass")
	}
	// CIDR 内但不匹配精确黑名单 → 放行。
	if denied, _ := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8"}, "10.200.3.4"); denied {
		t.Fatal("ip inside CIDR whitelist must pass")
	}
	// 白名单外 → 拒。
	if denied, reason := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8"}, "192.168.1.1"); !denied || reason != "ip_not_allowed" {
		t.Fatalf("ip outside whitelist must be denied: %v %s", denied, reason)
	}
	// 分层交集：层1=10.0.0.0/8、层2=10.1.0.0/16 → IP 必须同时命中两层
	//（10.0.5.5 ∈ 层1 但 ∉ 层2 → 拒；10.1.2.3 两层都命中 → 放行）。
	if denied, _ := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8|10.1.0.0/16"}, "10.0.5.5"); !denied {
		t.Fatal("must hit every non-empty whitelist layer")
	}
	if denied, _ := ipDenied(&Snapshot{IPWhitelistRule: "10.0.0.0/8|10.1.0.0/16"}, "10.1.2.3"); denied {
		t.Fatal("ip inside both layers must pass")
	}
	// 无规则 → 放行。
	if denied, _ := ipDenied(&Snapshot{}, "1.2.3.4"); denied {
		t.Fatal("no rules means no IP restriction")
	}
}

func TestIPEntryMatchesCIDRAndExact(t *testing.T) {
	if !ipEntryMatches("10.0.0.0/8", "10.1.2.3") {
		t.Fatal("CIDR must contain inner ip")
	}
	if ipEntryMatches("10.0.0.0/8", "11.1.2.3") {
		t.Fatal("CIDR must not contain outside ip")
	}
	if !ipEntryMatches("10.0.0.0/8", "10.0.0.1") {
		t.Fatal("CIDR must contain exact ip")
	}
	if !ipEntryMatches("192.168.1.5", "192.168.1.5") {
		t.Fatal("exact ip must match")
	}
	if ipEntryMatches("192.168.1.5", "192.168.1.6") {
		t.Fatal("exact ip must not match neighbor")
	}
}
