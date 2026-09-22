package auth

import (
	"testing"
	"time"
)

// The local recent-failure degradation penalty is scoped to intra-tier ordering: it may
// reorder credentials that already share one configured priority tier, but it must never
// change the tier itself. Upstream's configured-priority-only tier ordering wins.
func TestAuthPriorityIgnoresRecentFailures(t *testing.T) {
	auth := &Auth{
		Attributes: map[string]string{"priority": "20"},
	}
	if got := authPriority(auth); got != 20 {
		t.Fatalf("base priority = %d, want 20", got)
	}

	now := time.Now()
	// Record several failures in the current bucket.
	for i := 0; i < 4; i++ {
		auth.recordRecentRequest(now, false)
	}
	// One success so total is 5, fail rate 80% → the degradation is active.
	auth.recordRecentRequest(now, true)
	if penalty := authDegradationPenalty(auth); penalty == 0 {
		t.Fatal("degradation penalty = 0, want > 0 for a failing credential")
	}

	// The tier still comes from the configured priority: a recently failing credential
	// must not fall below a peer whose configured priority is lower.
	if got := authPriority(auth); got != 20 {
		t.Fatalf("degraded priority = %d, want the configured 20", got)
	}
}

func TestAuthPriorityNoDegradeOnSingleFailure(t *testing.T) {
	auth := &Auth{Attributes: map[string]string{"priority": "10"}}
	auth.recordRecentRequest(time.Now(), false)
	if got := authPriority(auth); got != 10 {
		t.Fatalf("single failure should not degrade: got %d", got)
	}
}

func TestOrderWithinTierDemotesDegradedCredentials(t *testing.T) {
	now := time.Now()
	healthy := &Auth{ID: "a-healthy", Attributes: map[string]string{"priority": "3"}}
	degraded := &Auth{ID: "b-degraded", Attributes: map[string]string{"priority": "3"}}
	for i := 0; i < 3; i++ {
		degraded.recordRecentRequest(now, false)
	}

	got := orderWithinTier([]*Auth{degraded, healthy})
	if len(got) != 2 || got[0] != healthy || got[1] != degraded {
		t.Fatalf("intra-tier order = %v, want the healthy peer first", authIDs(got))
	}
	// Both credentials stay in the tier: the penalty reorders, it never removes.
	if authPriority(healthy) != authPriority(degraded) {
		t.Fatalf("degradation changed the tier: %d vs %d", authPriority(healthy), authPriority(degraded))
	}
}

func TestOrderWithinTierLeavesHealthyTiersUntouched(t *testing.T) {
	first := &Auth{ID: "a-first", Attributes: map[string]string{"priority": "3"}}
	second := &Auth{ID: "b-second", Attributes: map[string]string{"priority": "3"}}
	got := orderWithinTier([]*Auth{first, second})
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("order = %v, want the input order preserved", authIDs(got))
	}
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			ids = append(ids, "<nil>")
			continue
		}
		ids = append(ids, auth.ID)
	}
	return ids
}
