package requestjournal

import (
	"testing"
	"time"
)

// chRec builds a journal Record with error/failure fields for chain tests.
func chRec(id, requestID string, ts int64, failed bool, provider, authIndex, model, protocol, kind, errBody string) Record {
	r := usRec(id, requestID, ts, failed, provider, authIndex, model, protocol)
	r.FailureKind = kind
	r.ErrorBody = errBody
	if failed {
		r.StatusCode = 500
	} else {
		r.StatusCode = 200
	}
	return r
}

// latRec builds a journal Record carrying latency/timeto-first-token for tests.
func latRec(id, requestID string, ts, latencyMs, ttftMs int64, failed bool) Record {
	r := chRec(id, requestID, ts, failed, "p", "a", "m", "http", "quota", "boom")
	r.LatencyMs = latencyMs
	r.TTFTMs = ttftMs
	return r
}

func TestBuildAttemptChainsOnlyHardFailed(t *testing.T) {
	// A resolved request must not surface as a chain; a hard-failed one must.
	chains := BuildAttemptChains([]Record{
		chRec("ok-100", "req-ok", 100, false, "p", "a", "m", "http", "", ""),
		chRec("bad-100", "req-bad", 100, true, "p", "a", "m", "http", "quota", "429 too many"),
	}, "", "")
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1 (only hard-failed)", len(chains))
	}
	if chains[0].RequestID != "req-bad" {
		t.Fatalf("chain request_id = %q, want req-bad", chains[0].RequestID)
	}
	if chains[0].FinalFailureKind != "quota" || chains[0].FinalErrorBody != "429 too many" {
		t.Fatalf("chain final failure = %q/%q, want quota/429 too many",
			chains[0].FinalFailureKind, chains[0].FinalErrorBody)
	}
}

func TestBuildAttemptChainsOrdersAttemptsByTime(t *testing.T) {
	chains := BuildAttemptChains([]Record{
		// newest-first input order must be reordered to oldest-first attempts.
		chRec("r-300", "req", 300, true, "p", "a", "m", "http", "quota", "final"),
		chRec("r-200", "req", 200, true, "p", "a", "m", "http", "network", "conn reset"),
		chRec("r-100", "req", 100, true, "p", "a", "m", "http", "quota", "first"),
	}, "", "")
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1", len(chains))
	}
	atts := chains[0].Attempts
	if len(atts) != 3 {
		t.Fatalf("got %d attempts, want 3", len(atts))
	}
	if atts[0].Timestamp.Unix() != 100 || atts[1].Timestamp.Unix() != 200 || atts[2].Timestamp.Unix() != 300 {
		t.Fatalf("attempts not in time order: %v", atts)
	}
	// The chain's final failure is the terminal attempt's.
	if chains[0].FinalFailureKind != "quota" || chains[0].FinalErrorBody != "final" {
		t.Fatalf("final failure = %q/%q, want quota/final", chains[0].FinalFailureKind, chains[0].FinalErrorBody)
	}
}

func TestBuildAttemptChainsPropagatesLatency(t *testing.T) {
	chains := BuildAttemptChains([]Record{
		latRec("r-100", "req", 100, 1200, 300, true),
		latRec("r-200", "req", 200, 500, 100, true),
	}, "", "")
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1", len(chains))
	}
	atts := chains[0].Attempts
	if len(atts) != 2 {
		t.Fatalf("got %d attempts, want 2", len(atts))
	}
	if atts[0].LatencyMs != 1200 || atts[0].TTFTMs != 300 {
		t.Fatalf("attempt[0] latency=%d ttft=%d, want 1200/300", atts[0].LatencyMs, atts[0].TTFTMs)
	}
	if atts[1].LatencyMs != 500 || atts[1].TTFTMs != 100 {
		t.Fatalf("attempt[1] latency=%d ttft=%d, want 500/100", atts[1].LatencyMs, atts[1].TTFTMs)
	}
}

func TestBuildAttemptChainsSwitchOnProviderOrAuthChange(t *testing.T) {
	chains := BuildAttemptChains([]Record{
		chRec("r-100", "req", 100, true, "p1", "a", "m", "http", "quota", "first"),
		chRec("r-200", "req", 200, true, "p1", "b", "m", "http", "quota", "cred switch"),
		chRec("r-300", "req", 300, true, "p2", "b", "m", "http", "network", "provider switch"),
	}, "", "")
	atts := chains[0].Attempts
	if len(atts) != 3 {
		t.Fatalf("got %d attempts, want 3", len(atts))
	}
	if atts[0].Switch {
		t.Fatal("first attempt must not be flagged as a switch")
	}
	// auth change (a→b) is a switch.
	if !atts[1].Switch {
		t.Fatal("attempt 2 changed auth_index, want switch=true")
	}
	// provider change (p1→p2) is a switch.
	if !atts[2].Switch {
		t.Fatal("attempt 3 changed provider, want switch=true")
	}
}

func TestBuildAttemptChainsModelChangeNotASwitch(t *testing.T) {
	chains := BuildAttemptChains([]Record{
		chRec("r-100", "req", 100, true, "p", "a", "m1", "http", "quota", "first"),
		chRec("r-200", "req", 200, true, "p", "a", "m2", "http", "quota", "model change"),
	}, "", "")
	atts := chains[0].Attempts
	if len(atts) != 2 {
		t.Fatalf("got %d attempts, want 2", len(atts))
	}
	if atts[1].Switch {
		t.Fatal("model-only change must not be flagged as a switch (ADR-0002)")
	}
}

func TestBuildAttemptChainsFiltersByProviderAndModel(t *testing.T) {
	recs := []Record{
		chRec("a-100", "req-a", 100, true, "p1", "a", "m1", "http", "quota", ""),
		chRec("b-100", "req-b", 100, true, "p2", "a", "m2", "http", "quota", ""),
	}
	if got := BuildAttemptChains(recs, "p1", ""); len(got) != 1 || got[0].RequestID != "req-a" {
		t.Fatalf("provider filter = %d chains, want 1 (req-a)", len(got))
	}
	if got := BuildAttemptChains(recs, "", "m2"); len(got) != 1 || got[0].RequestID != "req-b" {
		t.Fatalf("model filter = %d chains, want 1 (req-b)", len(got))
	}
}

func TestBuildAttemptChainsNewestFirstAndLimit(t *testing.T) {
	recs := []Record{
		chRec("r1-100", "req1", 100, true, "p", "a", "m", "http", "quota", ""),
		chRec("r2-200", "req2", 200, true, "p", "a", "m", "http", "quota", ""),
		chRec("r3-300", "req3", 300, true, "p", "a", "m", "http", "quota", ""),
	}
	chains := BuildAttemptChains(recs, "", "")
	if len(chains) != 3 {
		t.Fatalf("got %d chains, want 3", len(chains))
	}
	// newest final timestamp first.
	if chains[0].RequestID != "req3" || chains[1].RequestID != "req2" || chains[2].RequestID != "req1" {
		t.Fatalf("chains not newest-first: %v", chains)
	}
}

func TestBuildAttemptChainsStandaloneHardFailed(t *testing.T) {
	// Records without a RequestID are their own single-attempt chains.
	chains := BuildAttemptChains([]Record{
		chRec("s1", "", 100, true, "p", "a", "m", "http", "quota", "standalone fail"),
		chRec("s2", "", 200, false, "p", "a", "m", "http", "", ""),
	}, "", "")
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1 (standalone hard-failed)", len(chains))
	}
	if chains[0].AttemptCount != 1 {
		t.Fatalf("standalone chain attempt_count = %d, want 1", chains[0].AttemptCount)
	}
}

func TestAttemptChainsSinceLimit(t *testing.T) {
	// AttemptChainsSince honors the limit cap.
	dir := t.TempDir()
	Configure(dir, 30)
	t.Cleanup(func() { Configure("", 0) })
	for i := int64(1); i <= 5; i++ {
		rec := chRec("r", "req"+string(rune('0'+i)), time.Now().Add(-time.Duration(5-i)*time.Minute).Unix(),
			true, "p", "a", "m", "http", "quota", "")
		rec.ID = "r" + string(rune('0'+i)) + "-x"
		_ = appendRecord(rec)
	}
	chains, err := AttemptChainsSince(24*time.Hour, "", "", 3, ChainCursor{})
	if err != nil {
		t.Fatalf("AttemptChainsSince: %v", err)
	}
	if len(chains) > 3 {
		t.Fatalf("got %d chains, want <= 3 (limit)", len(chains))
	}
}

func TestAttemptChainsSinceBeforeCursor(t *testing.T) {
	dir := t.TempDir()
	Configure(dir, 30)
	t.Cleanup(func() { Configure("", 0) })
	now := time.Now()
	for i := int64(1); i <= 4; i++ {
		rec := chRec("r", "req"+string(rune('0'+i)), now.Add(-time.Duration(4-i)*time.Minute).Unix(),
			true, "p", "a", "m", "http", "quota", "")
		rec.ID = "r" + string(rune('0'+i)) + "-x"
		_ = appendRecord(rec)
	}
	// Newest-first without a cursor: req4, req3, req2, req1.
	all, err := AttemptChainsSince(24*time.Hour, "", "", 0, ChainCursor{})
	if err != nil {
		t.Fatalf("AttemptChainsSince: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d chains, want 4", len(all))
	}
	// Using req3's terminal timestamp as cursor should return only req2 and req1.
	older, err := AttemptChainsSince(24*time.Hour, "", "", 0,
		ChainCursor{FinalTimestamp: all[1].FinalTimestamp})
	if err != nil {
		t.Fatalf("AttemptChainsSince(cursor): %v", err)
	}
	if len(older) != 2 {
		t.Fatalf("cursor returned %d chains, want 2", len(older))
	}
	if older[0].RequestID != "req2" || older[1].RequestID != "req1" {
		t.Fatalf("cursor chains wrong: %v", older)
	}
}

func TestAttemptChainsSinceSameTimestampCursor(t *testing.T) {
	// Chains sharing a terminal timestamp paginate by the RequestID tiebreak:
	// no chain is skipped or duplicated across the page boundary.
	dir := t.TempDir()
	Configure(dir, 30)
	t.Cleanup(func() { Configure("", 0) })
	ts := time.Now()
	for i := int64(1); i <= 3; i++ {
		rec := chRec("r", "req"+string(rune('0'+i)), ts.Unix(), true, "p", "a", "m", "http", "quota", "")
		rec.ID = "r" + string(rune('0'+i)) + "-x"
		_ = appendRecord(rec)
	}
	// First page: limit 2 of the 3 same-instant chains, RequestID desc.
	page, err := AttemptChainsSince(24*time.Hour, "", "", 2, ChainCursor{})
	if err != nil {
		t.Fatalf("AttemptChainsSince: %v", err)
	}
	if len(page) != 2 || page[0].RequestID != "req3" || page[1].RequestID != "req2" {
		t.Fatalf("first page = %v, want [req3 req2]", page)
	}
	last := page[len(page)-1]
	next, err := AttemptChainsSince(24*time.Hour, "", "", 0,
		ChainCursor{FinalTimestamp: last.FinalTimestamp, RequestID: last.RequestID})
	if err != nil {
		t.Fatalf("AttemptChainsSince(cursor): %v", err)
	}
	if len(next) != 1 || next[0].RequestID != "req1" {
		t.Fatalf("next page = %v, want [req1]", next)
	}
}
