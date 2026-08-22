package requestjournal

import (
	"os"
	"sort"
	"strings"
	"time"
)

// Attempt is one upstream attempt within a user request's chain. Switch is true
// when this attempt differs from the previous one by provider or auth_index —
// i.e. the request rotated provider or credential here. Model changes are not
// flagged (no model downgrade today). See ADR-0002.
type Attempt struct {
	Timestamp   time.Time `json:"timestamp"`
	Provider    string    `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	Model       string    `json:"model"`
	Protocol    string    `json:"protocol"`
	Failed      bool      `json:"failed"`
	StatusCode  int       `json:"status_code"`
	FailureKind string    `json:"failure_kind,omitempty"`
	ErrorBody   string    `json:"error_body,omitempty"`
	LatencyMs   int64     `json:"latency_ms,omitempty"`
	TTFTMs      int64     `json:"ttft_ms,omitempty"`
	Switch      bool      `json:"switch"`
}

// RequestChain is one user request's ordered attempt timeline. Only hard-failed
// requests are surfaced as chains (the failure waterfall). FinalTimestamp is the
// terminal (deciding) attempt's timestamp, used for newest-first ordering.
type RequestChain struct {
	RequestID        string    `json:"request_id"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	Protocol         string    `json:"protocol"`
	FinalTimestamp   time.Time `json:"final_timestamp"`
	AttemptCount     int       `json:"attempt_count"`
	FinalFailureKind string    `json:"final_failure_kind,omitempty"`
	FinalErrorBody   string    `json:"final_error_body,omitempty"`
	Attempts         []Attempt `json:"attempts"`
}

// BuildAttemptChains groups records by RequestID, orders each group's attempts
// by timestamp, and returns a chain for every hard-failed request (its
// latest-timestamped attempt failed — the same terminal rule as ADR-0001). A
// record without a RequestID is a standalone single-attempt request. Provider /
// model filters narrow which chains are returned. The terminal attempt of a
// chain is always the final (deciding) attempt, so its failure attributes the
// chain's outcome.
func BuildAttemptChains(records []Record, providerFilter, modelFilter string) []RequestChain {
	groups := groupRecordsByRequest(records)

	var chains []RequestChain
	for _, attempts := range groups {
		// attempts is in timestamp order; the terminal attempt is the last one.
		term := attempts[len(attempts)-1]
		if !term.Failed {
			continue // only hard-failed requests surface as chains
		}
		if providerFilter != "" && !strings.EqualFold(term.Provider, providerFilter) {
			continue
		}
		if modelFilter != "" && !strings.EqualFold(term.Model, modelFilter) {
			continue
		}
		chain := RequestChain{
			RequestID:        term.RequestID,
			Model:            term.Model,
			Provider:         term.Provider,
			Protocol:         term.Protocol,
			FinalTimestamp:   term.Timestamp,
			AttemptCount:     len(attempts),
			FinalFailureKind: term.FailureKind,
			FinalErrorBody:   term.ErrorBody,
		}
		for i, a := range attempts {
			at := Attempt{
				Timestamp:   a.Timestamp,
				Provider:    a.Provider,
				AuthIndex:   a.AuthIndex,
				Model:       a.Model,
				Protocol:    a.Protocol,
				Failed:      a.Failed,
				StatusCode:  a.StatusCode,
				FailureKind: a.FailureKind,
				ErrorBody:   a.ErrorBody,
				LatencyMs:   a.LatencyMs,
				TTFTMs:      a.TTFTMs,
			}
			if i > 0 {
				prev := attempts[i-1]
				at.Switch = a.Provider != prev.Provider || a.AuthIndex != prev.AuthIndex
			}
			chain.Attempts = append(chain.Attempts, at)
		}
		chains = append(chains, chain)
	}

	sort.Slice(chains, func(i, j int) bool {
		if !chains[i].FinalTimestamp.Equal(chains[j].FinalTimestamp) {
			return chains[i].FinalTimestamp.After(chains[j].FinalTimestamp)
		}
		return chains[i].RequestID > chains[j].RequestID
	})
	return chains
}

// groupRecordsByRequest partitions records by RequestID and returns each group's
// attempts sorted by timestamp ascending. Records without a RequestID are their
// own single-element group. The order of groups is unspecified.
func groupRecordsByRequest(records []Record) [][]Record {
	groups := map[string][]Record{}
	order := []string{}
	for i := range records {
		rec := records[i]
		key := rec.RequestID
		if key == "" {
			// Standalone single-attempt request: unique synthetic key per index.
			key = standaloneKey(i)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], rec)
	}
	out := make([][]Record, 0, len(order))
	for _, key := range order {
		g := groups[key]
		sort.Slice(g, func(i, j int) bool { return g[i].Timestamp.Before(g[j].Timestamp) })
		out = append(out, g)
	}
	return out
}

// ChainCursor continues pagination after the last returned chain. Chains are
// ordered newest-first by FinalTimestamp, with RequestID descending as the
// tiebreak, so the pair forms a total order; pass the previous page's last
// chain here to load the next page without skipping or duplicating chains that
// share a terminal timestamp.
type ChainCursor struct {
	FinalTimestamp time.Time
	RequestID      string
}

// IsZero reports whether the cursor is unset.
func (c ChainCursor) IsZero() bool {
	return c.FinalTimestamp.IsZero()
}

// afterCursor reports whether ch comes strictly after cursor in the
// newest-first ordering (FinalTimestamp desc, then RequestID desc).
func afterCursor(ch RequestChain, cursor ChainCursor) bool {
	if ch.FinalTimestamp.Before(cursor.FinalTimestamp) {
		return true
	}
	return ch.FinalTimestamp.Equal(cursor.FinalTimestamp) && ch.RequestID < cursor.RequestID
}

// AttemptChainsSince reads the durable journal over a rolling window and returns
// hard-failed request chains, newest first, capped at limit. providerFilter and
// modelFilter narrow which chains are returned. A non-zero cursor returns only
// chains strictly after it in the newest-first ordering (load-more pagination).
func AttemptChainsSince(window time.Duration, providerFilter, modelFilter string, limit int, cursor ChainCursor) ([]RequestChain, error) {
	records, err := readWindow(window)
	if err != nil {
		return nil, err
	}
	chains := BuildAttemptChains(records, providerFilter, modelFilter)
	if !cursor.IsZero() {
		kept := chains[:0]
		for _, ch := range chains {
			if afterCursor(ch, cursor) {
				kept = append(kept, ch)
			}
		}
		chains = kept
	}
	if limit > 0 && len(chains) > limit {
		chains = chains[:limit]
	}
	return chains, nil
}

// readWindow reads every journal record with Timestamp within the last `window`
// duration. It reads only the day files that overlap the window (same approach
// as UserSideAvailabilitySince) and skips already-seen day files.
func readWindow(window time.Duration) ([]Record, error) {
	now := time.Now()
	start := now.Add(-window)
	var records []Record
	seen := map[string]bool{}
	for d := start.UTC(); !d.After(now.UTC()); d = d.Add(24 * time.Hour) {
		path := dayFile(d)
		if seen[path] {
			continue
		}
		seen[path] = true
		recs, err := readFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for i := range recs {
			if recs[i].Timestamp.Before(start) {
				continue
			}
			records = append(records, recs[i])
		}
	}
	return records, nil
}
