package helps

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStallWatchdogFiresWhenSilentAfterFirstProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stalled atomic.Bool
	w := NewStallWatchdog(ctx, 30*time.Millisecond, func() { stalled.Store(true) })
	defer w.Stop()

	// Before the first Progress the watchdog is not armed: silence must not fire.
	time.Sleep(60 * time.Millisecond)
	if stalled.Load() {
		t.Fatal("stall watchdog fired before first progress (must arm on first progress)")
	}

	// Arm it with one progress event, then go silent: the timer fires.
	w.Progress()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stalled.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !stalled.Load() {
		t.Fatal("stall watchdog did not fire after first progress went silent")
	}
	if !w.Stalled() {
		t.Fatal("Stalled() = false after timeout fired")
	}
}

func TestIsStreamEventLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{line: "data: {\"type\":\"message_start\"}", want: true},
		{line: "event: message_start", want: true},
		{line: "id: 1", want: true},
		{line: "{\"type\":\"ping\"}", want: true},
		{line: ": keep-alive", want: false},
		{line: ": ping", want: false},
		{line: "", want: false},
		{line: "   ", want: false},
	}
	for _, tc := range tests {
		if got := IsStreamEventLine([]byte(tc.line)); got != tc.want {
			t.Fatalf("IsStreamEventLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestStallWatchdogProgressResetsTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stalled atomic.Bool
	w := NewStallWatchdog(ctx, 40*time.Millisecond, func() { stalled.Store(true) })
	defer w.Stop()

	// Keep sending progress: the watchdog must never fire.
	for i := 0; i < 20; i++ {
		w.Progress()
		time.Sleep(10 * time.Millisecond)
	}
	if stalled.Load() {
		t.Fatal("stall watchdog fired despite ongoing progress")
	}
	if w.Stalled() {
		t.Fatal("Stalled() = true despite ongoing progress")
	}
}

func TestStallWatchdogDisabledWhenTimeoutNonPositive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewStallWatchdog(ctx, 0, func() { t.Fatal("onStall must not run when disabled") })
	defer w.Stop()
	time.Sleep(10 * time.Millisecond)
	if w.Stalled() {
		t.Fatal("Stalled() = true on disabled watchdog")
	}
}

func TestStallWatchdogStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stalled atomic.Bool
	w := NewStallWatchdog(ctx, time.Hour, func() { stalled.Store(true) })
	cancel()
	w.Stop() // must not deadlock
	if stalled.Load() {
		t.Fatal("onStall ran on context cancel")
	}
}

func TestResolveStallTimeout(t *testing.T) {
	if got := ResolveStallTimeout(0); got != 30*time.Second {
		t.Fatalf("default = %v, want 30s", got)
	}
	if got := ResolveStallTimeout(15); got != 15*time.Second {
		t.Fatalf("15 = %v, want 15s", got)
	}
	if got := ResolveStallTimeout(-1); got != 0 {
		t.Fatalf("-1 = %v, want 0 (disabled)", got)
	}
}
