package requestjournal

import (
	"testing"
	"time"
)

func TestUserSideAvailabilitySinceReadsWindow(t *testing.T) {
	// Point the journal at a temp dir and restore global state afterwards.
	oldRoot := rootDir
	oldEnabled := enabled
	oldMaxDays := maxDays
	t.Cleanup(func() {
		mu.Lock()
		rootDir = oldRoot
		enabled = oldEnabled
		maxDays = oldMaxDays
		mu.Unlock()
	})

	dir := t.TempDir()
	Configure(dir, 30)

	now := time.Now()
	// A request resolved after one retry, inside the window.
	if err := appendRecord(usRec("w1-1", "w1", now.Add(-2*time.Minute).Unix(), true, "p", "a", "m", "http")); err != nil {
		t.Fatal(err)
	}
	if err := appendRecord(usRec("w1-2", "w1", now.Add(-time.Minute).Unix(), false, "p", "a", "m", "http")); err != nil {
		t.Fatal(err)
	}
	// A hard-failed request, inside the window.
	if err := appendRecord(usRec("w2-1", "w2", now.Add(-30*time.Second).Unix(), true, "p", "a", "m", "http")); err != nil {
		t.Fatal(err)
	}

	agg, err := UserSideAvailabilitySince(24 * time.Hour)
	if err != nil {
		t.Fatalf("UserSideAvailabilitySince: %v", err)
	}
	e := edge("p", "a", "m", "http")
	got := agg[e]
	if got.Resolved != 1 || got.HardFailed != 1 {
		t.Fatalf("edge = %+v, want 1 resolved / 1 hard-failed", got)
	}
}

func TestUserSideAvailabilitySinceExcludesOutsideWindow(t *testing.T) {
	oldRoot := rootDir
	oldEnabled := enabled
	oldMaxDays := maxDays
	t.Cleanup(func() {
		mu.Lock()
		rootDir = oldRoot
		enabled = oldEnabled
		maxDays = oldMaxDays
		mu.Unlock()
	})

	dir := t.TempDir()
	Configure(dir, 30)

	now := time.Now()
	// A record from well before the window start must be ignored.
	if err := appendRecord(usRec("old", "old", now.Add(-48*time.Hour).Unix(), true, "p", "a", "m", "http")); err != nil {
		t.Fatal(err)
	}

	agg, err := UserSideAvailabilitySince(24 * time.Hour)
	if err != nil {
		t.Fatalf("UserSideAvailabilitySince: %v", err)
	}
	if len(agg) != 0 {
		t.Fatalf("expected no edges in window, got %+v", agg)
	}
}
