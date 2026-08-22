package helps

import (
	"bytes"
	"context"
	"sync/atomic"
	"time"
)

// ResolveStallTimeout resolves the configured stream stall timeout.
// 0 uses the 30s default; negative disables the stall guard.
func ResolveStallTimeout(seconds int) time.Duration {
	if seconds < 0 {
		return 0
	}
	if seconds == 0 {
		return 30 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// StallWatchdog detects a stream that has stopped producing event lines. The
// timer starts on the first Progress() call (i.e. once the stream has produced
// its first bytes) and every subsequent Progress() resets it. When the timeout
// fires, Stalled() becomes true and the onStall callback runs (the executor
// uses it to close the response body, which unblocks the scanner so the dead
// stream can be failed as retryable instead of hanging forever).
type StallWatchdog struct {
	timeout  time.Duration
	stalled  atomic.Bool
	progress chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

// NewStallWatchdog creates a stall watchdog. A non-positive timeout disables it.
func NewStallWatchdog(ctx context.Context, timeout time.Duration, onStall func()) *StallWatchdog {
	w := &StallWatchdog{
		timeout:  timeout,
		progress: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if timeout <= 0 {
		close(w.done)
		return w
	}
	go w.run(ctx, onStall)
	return w
}

func (w *StallWatchdog) run(ctx context.Context, onStall func()) {
	defer close(w.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-w.progress:
			if timer == nil {
				// First progress arms the watchdog: stall detection starts only
				// after the stream has produced its first bytes, so the bootstrap
				// phase stays governed by the first-chunk timeout instead.
				timer = time.NewTimer(w.timeout)
				timerC = timer.C
			} else {
				timer.Reset(w.timeout)
			}
		case <-timerC:
			if w.stalled.CompareAndSwap(false, true) {
				if onStall != nil {
					onStall()
				}
			}
			return
		}
	}
}

// Progress resets the stall timer. Call it for every event line received from
// the upstream stream. Keep-alive comment lines (": ping") and empty frames
// prove the connection is alive but not that the model is producing output, so
// they should NOT be reported as progress.
func (w *StallWatchdog) Progress() {
	if w == nil || w.timeout <= 0 {
		return
	}
	select {
	case w.progress <- struct{}{}:
	default:
	}
}

// Stalled reports whether the stall timeout fired.
func (w *StallWatchdog) Stalled() bool {
	return w != nil && w.stalled.Load()
}

// Stop terminates the watchdog and waits for its goroutine to exit.
func (w *StallWatchdog) Stop() {
	if w == nil || w.timeout <= 0 {
		return
	}
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// IsStreamEventLine reports whether an SSE line carries an event (data:/event:/
// id: prefixes or a bare JSON payload). Comment lines (": ping") and empty
// frames only prove the connection is alive, not that the model is producing
// output, so they must not count as stall progress.
func IsStreamEventLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] == ':' {
		return false
	}
	return true
}
