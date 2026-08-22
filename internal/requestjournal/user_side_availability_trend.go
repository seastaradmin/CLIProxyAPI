package requestjournal

import (
	"time"
)

// TrendBucket is user-side availability over one time bucket: resolved counts
// user requests whose terminal attempt succeeded, hard-failed those that
// exhausted retries/rotation with an error. Availability is resolved/total.
type TrendBucket struct {
	Time         time.Time `json:"time"` // bucket start, UTC
	Resolved     int64     `json:"resolved"`
	HardFailed   int64     `json:"hard_failed"`
	Availability float64   `json:"availability"`
}

// bucketSizeForWindow maps a window to a time-bucket size so each window yields
// a stable, readable number of data points. See ADR-0002.
func bucketSizeForWindow(window time.Duration) time.Duration {
	switch {
	case window <= 15*time.Minute:
		return time.Minute
	case window <= time.Hour:
		return 5 * time.Minute
	case window <= 6*time.Hour:
		return 15 * time.Minute
	case window <= 24*time.Hour:
		return time.Hour
	case window <= 7*24*time.Hour:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// BuildAvailabilityTrend groups records by RequestID, decides each request's
// outcome by its terminal (latest) attempt, then buckets requests by their
// terminal attempt's timestamp. Buckets are contiguous and aligned to the
// window start; empty buckets appear with zero counts. Filters narrow which
// requests are counted.
func BuildAvailabilityTrend(records []Record, window time.Duration, now time.Time, filters UserSideFilters) []TrendBucket {
	bucketSize := bucketSizeForWindow(window)
	if bucketSize <= 0 {
		return nil
	}
	start := now.Add(-window)

	// Count outcomes per aligned bucket, using each request's terminal attempt.
	counts := map[int64]TrendBucket{}
	for _, idx := range latestAttemptByRequest(records) {
		rec := &records[idx]
		edge := CapabilityEdgeKey{
			Provider:  rec.Provider,
			AuthIndex: rec.AuthIndex,
			Model:     rec.Model,
			Protocol:  rec.Protocol,
		}
		if !filters.MatchEdge(edge) {
			continue
		}
		bucketStart := alignBucket(rec.Timestamp, start, bucketSize)
		if bucketStart.Before(start) {
			continue
		}
		b := counts[bucketStart.Unix()]
		if rec.Failed {
			b.HardFailed++
		} else {
			b.Resolved++
		}
		counts[bucketStart.Unix()] = b
	}

	// Emit contiguous buckets covering the window.
	out := make([]TrendBucket, 0)
	for t := start; t.Before(now); t = t.Add(bucketSize) {
		b := counts[t.Unix()]
		b.Time = t.UTC()
		b.Availability = AvailabilityRatio(b.Resolved, b.Resolved+b.HardFailed)
		out = append(out, b)
	}
	return out
}

// alignBucket floors ts to the bucket boundary within the window-aligned series.
func alignBucket(ts, start time.Time, bucketSize time.Duration) time.Time {
	delta := ts.Sub(start)
	if delta < 0 {
		return start.Add(-bucketSize)
	}
	idx := int64(delta / bucketSize)
	return start.Add(time.Duration(idx) * bucketSize)
}

// AvailabilityTrendSince reads the durable journal over a rolling window and
// returns a contiguous, aligned time-bucketed user-side availability series.
func AvailabilityTrendSince(window time.Duration, filters UserSideFilters) ([]TrendBucket, error) {
	records, err := readWindow(window)
	if err != nil {
		return nil, err
	}
	return BuildAvailabilityTrend(records, window, time.Now(), filters), nil
}
