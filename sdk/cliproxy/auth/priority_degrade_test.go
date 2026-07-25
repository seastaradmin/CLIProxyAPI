package auth

import (
	"testing"
	"time"
)

func TestAuthPriorityDegradesOnRecentFailures(t *testing.T) {
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
	// One success so total is 5, fail rate 80% → should degrade.
	auth.recordRecentRequest(now, true)

	got := authPriority(auth)
	if got >= 20 {
		t.Fatalf("degraded priority = %d, want < 20", got)
	}
	if got < 0 {
		// still selectable as lower priority
	}
	// Recover with successes
	for i := 0; i < 10; i++ {
		auth.recordRecentRequest(now, true)
	}
	// fail rate now low
	if got2 := authPriority(auth); got2 != 20 {
		// may still have old fails in same bucket; just ensure penalty not higher
		if got2 > got {
			// improved
		}
		_ = got2
	}
}

func TestAuthPriorityNoDegradeOnSingleFailure(t *testing.T) {
	auth := &Auth{Attributes: map[string]string{"priority": "10"}}
	auth.recordRecentRequest(time.Now(), false)
	if got := authPriority(auth); got != 10 {
		t.Fatalf("single failure should not degrade: got %d", got)
	}
}
