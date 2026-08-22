package requestjournal

import (
	"testing"
	"time"
)

func edge(provider, authIndex, model, protocol string) CapabilityEdgeKey {
	return CapabilityEdgeKey{Provider: provider, AuthIndex: authIndex, Model: model, Protocol: protocol}
}

// usRec builds a journal Record. requestID "" exercises the standalone path.
func usRec(id, requestID string, ts int64, failed bool, provider, authIndex, model, protocol string) Record {
	return Record{
		ID:        id,
		RequestID: requestID,
		Timestamp: time.Unix(ts, 0),
		Failed:    failed,
		Provider:  provider,
		AuthIndex: authIndex,
		Model:     model,
		Protocol:  protocol,
	}
}

func TestAggregateUserSideAvailabilityRetryThenSuccess(t *testing.T) {
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req1-100", "req1", 100, true, "p", "a", "m", "http"),
		usRec("req1-200", "req1", 200, false, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 1 || got.HardFailed != 0 {
		t.Fatalf("edge = %+v, want 1 resolved / 0 hard-failed", got)
	}
}

func TestAggregateUserSideAvailabilityAllAttemptsFailed(t *testing.T) {
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req2-100", "req2", 100, true, "p", "a", "m", "http"),
		usRec("req2-200", "req2", 200, true, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 0 || got.HardFailed != 1 {
		t.Fatalf("edge = %+v, want 0 resolved / 1 hard-failed", got)
	}
}

func TestAggregateUserSideAvailabilityLatestAttemptDecides(t *testing.T) {
	// Latest attempt fails even though an earlier one succeeded → hard-failed
	// (the fan-out approximation from ADR-0001).
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req3-100", "req3", 100, false, "p", "a", "m", "http"),
		usRec("req3-200", "req3", 200, true, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 0 || got.HardFailed != 1 {
		t.Fatalf("edge = %+v, want 0 resolved / 1 hard-failed", got)
	}
}

func TestAggregateUserSideAvailabilityInputOrderIndependent(t *testing.T) {
	// Records arrive newest-first from the journal; the outcome must be the
	// same as if they arrived oldest-first.
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req4-200", "req4", 200, true, "p", "a", "m", "http"),
		usRec("req4-100", "req4", 100, false, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 0 || got.HardFailed != 1 {
		t.Fatalf("edge = %+v, want 0 resolved / 1 hard-failed", got)
	}
}

func TestAggregateUserSideAvailabilityCrossDayGrouping(t *testing.T) {
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req6-day1", "req6", 100000, true, "p", "a", "m", "http"),
		usRec("req6-day2", "req6", 100000+90000, false, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 1 || got.HardFailed != 0 {
		t.Fatalf("edge = %+v, want 1 resolved / 0 hard-failed across days", got)
	}
}

func TestAggregateUserSideAvailabilityMissingRequestIDStandalone(t *testing.T) {
	e := edge("p", "a", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("s1", "", 100, false, "p", "a", "m", "http"),
		usRec("s2", "", 200, true, "p", "a", "m", "http"),
	})
	got := agg[e]
	if got.Resolved != 1 || got.HardFailed != 1 {
		t.Fatalf("edge = %+v, want 1 resolved / 1 hard-failed for standalone records", got)
	}
}

func TestAggregateUserSideAvailabilityAttributesToTerminalEdge(t *testing.T) {
	// A request that rotated off edge (p,a) and resolved on (p,b) is attributed
	// to the terminal edge (p,b) only.
	eA := edge("p", "a", "m", "http")
	eB := edge("p", "b", "m", "http")
	agg := AggregateUserSideAvailability([]Record{
		usRec("req5-100", "req5", 100, true, "p", "a", "m", "http"),
		usRec("req5-200", "req5", 200, false, "p", "b", "m", "http"),
	})
	if got := agg[eB]; got.Resolved != 1 || got.HardFailed != 0 {
		t.Fatalf("terminal edge = %+v, want 1 resolved / 0 hard-failed", got)
	}
	if got, ok := agg[eA]; ok {
		t.Fatalf("non-terminal edge should be absent, got %+v", got)
	}
}
