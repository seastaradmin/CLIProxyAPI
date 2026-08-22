package requestjournal

import (
	"strconv"
	"strings"
	"time"
)

// CapabilityEdgeKey identifies one channel × credential × model × protocol
// route, the aggregation dimension for user-side availability. It mirrors the
// capability edge vocabulary from CONTEXT.md and the existing availability
// model; the journal records carry Provider/AuthIndex/Model/Protocol but no
// channel id, so these four fields define the edge key.
type CapabilityEdgeKey struct {
	Provider  string
	AuthIndex string
	Model     string
	Protocol  string
}

// UserSideFilters narrows which requests are counted in user-side availability
// aggregations and trends. Empty fields match everything.
type UserSideFilters struct {
	Provider  string
	AuthIndex string
	Model     string
	Protocol  string
}

// MatchEdge reports whether the edge passes the filters.
func (f UserSideFilters) MatchEdge(edge CapabilityEdgeKey) bool {
	if f.Provider != "" && !strings.EqualFold(edge.Provider, f.Provider) {
		return false
	}
	if f.AuthIndex != "" && edge.AuthIndex != f.AuthIndex {
		return false
	}
	if f.Model != "" && edge.Model != f.Model {
		return false
	}
	if f.Protocol != "" && !strings.EqualFold(edge.Protocol, f.Protocol) {
		return false
	}
	return true
}

// AvailabilityRatio returns resolved/total, or 0 when total is zero.
func AvailabilityRatio(resolved, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(resolved) / float64(total)
}

// latestAttemptByRequest returns, per RequestID, the index of the attempt with
// the newest timestamp encountered so far. Records without a RequestID are
// keyed by a synthetic unique-per-index key so they never collide with grouped
// requests.
func latestAttemptByRequest(records []Record) map[string]int {
	latest := map[string]int{}
	for i := range records {
		rec := &records[i]
		key := rec.RequestID
		if key == "" {
			key = standaloneKey(i)
		}
		idx, ok := latest[key]
		if !ok || rec.Timestamp.After(records[idx].Timestamp) {
			latest[key] = i
		}
	}
	return latest
}

func standaloneKey(i int) string {
	return "\x00standalone\x00" + strconv.Itoa(i)
}

// UserSideAvailability holds the request-level outcome counts for one
// capability edge. Resolved counts user requests whose latest upstream attempt
// succeeded (no hard error delivered); HardFailed counts those that exhausted
// retries/rotation with an error. See ADR-0001.
type UserSideAvailability struct {
	Resolved   int64
	HardFailed int64
}

// AggregateUserSideAvailability groups journal records by RequestID, decides
// each user request's final outcome by its latest-timestamped attempt, and
// aggregates the outcomes per capability edge. Records without a RequestID are
// treated as standalone single-attempt requests. Each request is attributed to
// the capability edge of its terminal (deciding) attempt, so a request that
// rotated off one edge and resolved on another is counted only on the terminal
// edge.
func AggregateUserSideAvailability(records []Record) map[CapabilityEdgeKey]UserSideAvailability {
	out := map[CapabilityEdgeKey]UserSideAvailability{}
	for _, idx := range latestAttemptByRequest(records) {
		rec := &records[idx]
		edge := CapabilityEdgeKey{
			Provider:  rec.Provider,
			AuthIndex: rec.AuthIndex,
			Model:     rec.Model,
			Protocol:  rec.Protocol,
		}
		av := out[edge]
		if rec.Failed {
			av.HardFailed++
		} else {
			av.Resolved++
		}
		out[edge] = av
	}
	return out
}

// UserSideAvailabilitySince reads the durable request journal over a rolling
// window (default caller-provided, typically 24h) and aggregates user-side
// availability per capability edge. It reads only the day files that overlap
// the window, then delegates grouping to AggregateUserSideAvailability. Records
// are collected regardless of List's record cap, since a full window must be
// grouped, not a recent sample.
func UserSideAvailabilitySince(window time.Duration) (map[CapabilityEdgeKey]UserSideAvailability, error) {
	records, err := readWindow(window)
	if err != nil {
		return nil, err
	}
	return AggregateUserSideAvailability(records), nil
}
