package management

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/requestjournal"
)

// userSideEdge is one capability edge's request-level availability for the
// management UI. Availability is resolved/total over the window.
type userSideEdge struct {
	Provider     string  `json:"provider"`
	AuthIndex    string  `json:"auth_index"`
	Model        string  `json:"model"`
	Protocol     string  `json:"protocol"`
	Resolved     int64   `json:"resolved"`
	HardFailed   int64   `json:"hard_failed"`
	Total        int64   `json:"total"`
	Availability float64 `json:"availability"`
}

// parseUserSideFilters reads the shared provider / auth_index / model / protocol
// query filters, trimmed; empty query values stay empty (match everything).
func parseUserSideFilters(c *gin.Context) requestjournal.UserSideFilters {
	return requestjournal.UserSideFilters{
		Provider:  strings.TrimSpace(c.Query("provider")),
		AuthIndex: strings.TrimSpace(c.Query("auth_index")),
		Model:     strings.TrimSpace(c.Query("model")),
		Protocol:  strings.TrimSpace(c.Query("protocol")),
	}
}

// GetUserSideAvailability returns user-side availability per capability edge:
// the share of user requests that reached upstream and ultimately resolved
// without a hard error, derived from the request journal. See ADR-0001.
// Query:
//   - window: duration string (default 24h)
//   - provider / auth_index / model / protocol: optional filters
func (h *Handler) GetUserSideAvailability(c *gin.Context) {
	window := parseUserSideWindow(c.Query("window"))
	filters := parseUserSideFilters(c)

	agg, err := requestjournal.UserSideAvailabilitySince(window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	now := time.Now()

	edges, totalResolved, totalHardFailed := buildUserSideAvailabilityEdges(agg, filters)

	c.JSON(http.StatusOK, gin.H{
		"window":               window.String(),
		"generated_at":         now,
		"overall_availability": requestjournal.AvailabilityRatio(totalResolved, totalResolved+totalHardFailed),
		"total_requests":       totalResolved + totalHardFailed,
		"total_resolved":       totalResolved,
		"total_hard_failed":    totalHardFailed,
		"durable":              requestjournal.Enabled(),
		"edges":                edges,
	})
}

// buildUserSideAvailabilityEdges converts an aggregation map into a sorted,
// filterable slice of per-capability-edge results and returns overall totals.
func buildUserSideAvailabilityEdges(agg map[requestjournal.CapabilityEdgeKey]requestjournal.UserSideAvailability,
	filters requestjournal.UserSideFilters) (edges []userSideEdge, totalResolved, totalHardFailed int64) {
	for key, av := range agg {
		if !filters.MatchEdge(key) {
			continue
		}
		edge := userSideEdge{
			Provider:   key.Provider,
			AuthIndex:  key.AuthIndex,
			Model:      key.Model,
			Protocol:   key.Protocol,
			Resolved:   av.Resolved,
			HardFailed: av.HardFailed,
		}
		edge.Total = edge.Resolved + edge.HardFailed
		edge.Availability = requestjournal.AvailabilityRatio(edge.Resolved, edge.Total)
		edges = append(edges, edge)
		totalResolved += av.Resolved
		totalHardFailed += av.HardFailed
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].HardFailed != edges[j].HardFailed {
			return edges[i].HardFailed > edges[j].HardFailed
		}
		if edges[i].Provider != edges[j].Provider {
			return edges[i].Provider < edges[j].Provider
		}
		if edges[i].Model != edges[j].Model {
			return edges[i].Model < edges[j].Model
		}
		return edges[i].AuthIndex < edges[j].AuthIndex
	})
	return edges, totalResolved, totalHardFailed
}

// parseUserSideWindow parses a window duration string, defaulting to 24h and
// capped at the journal retention so reads stay bounded.
func parseUserSideWindow(raw string) time.Duration {
	const (
		defaultUserSideWindow = 24 * time.Hour
		maxUserSideWindow     = 30 * 24 * time.Hour
	)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultUserSideWindow
	}
	d, err := parseDurationWithDays(raw)
	if err != nil || d <= 0 {
		return defaultUserSideWindow
	}
	if d > maxUserSideWindow {
		return maxUserSideWindow
	}
	return d
}

// GetUserSideRequestChains returns hard-failed user requests as ordered attempt
// chains, newest first, for the failure waterfall. Each chain shows every
// upstream attempt the request made (call → retry → error → switch
// provider/credential) so an operator can see why it ultimately failed. See
// ADR-0002.
// Query:
//   - window: duration string (default 24h)
//   - provider / model: optional filters
//   - limit: max number of chains (default 200)
//   - before: RFC3339(Nano) cursor timestamp; only chains after the cursor's
//     ordering position are returned (load-more)
//   - before_request_id: the cursor chain's request_id, required when several
//     chains share the cursor timestamp
func (h *Handler) GetUserSideRequestChains(c *gin.Context) {
	window := parseUserSideWindow(c.Query("window"))
	providerFilter := strings.TrimSpace(c.Query("provider"))
	modelFilter := strings.TrimSpace(c.Query("model"))
	limit := 200
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	var cursor requestjournal.ChainCursor
	if raw := strings.TrimSpace(c.Query("before")); raw != "" {
		if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			cursor.FinalTimestamp = ts
			cursor.RequestID = strings.TrimSpace(c.Query("before_request_id"))
		}
	}

	chains, err := requestjournal.AttemptChainsSince(window, providerFilter, modelFilter, limit, cursor)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if chains == nil {
		chains = []requestjournal.RequestChain{}
	}
	hasMore := false
	if len(chains) > 0 {
		last := chains[len(chains)-1]
		more, errMore := requestjournal.AttemptChainsSince(window, providerFilter, modelFilter, 1,
			requestjournal.ChainCursor{FinalTimestamp: last.FinalTimestamp, RequestID: last.RequestID})
		if errMore == nil && len(more) > 0 {
			hasMore = true
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"window":       window.String(),
		"generated_at": time.Now(),
		"durable":      requestjournal.Enabled(),
		"count":        len(chains),
		"has_more":     hasMore,
		"chains":       chains,
	})
}

// GetUserSideAvailabilityTrend returns user-side availability bucketed over the
// window, so the UI can plot the resolve rate over time. Bucket size follows the
// window mapping in ADR-0002. See ADR-0002.
// Query:
//   - window: duration string (default 24h)
//   - provider / auth_index / model / protocol: optional filters
func (h *Handler) GetUserSideAvailabilityTrend(c *gin.Context) {
	window := parseUserSideWindow(c.Query("window"))
	filters := parseUserSideFilters(c)

	buckets, err := requestjournal.AvailabilityTrendSince(window, filters)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if buckets == nil {
		buckets = []requestjournal.TrendBucket{}
	}
	c.JSON(http.StatusOK, gin.H{
		"window":       window.String(),
		"generated_at": time.Now(),
		"durable":      requestjournal.Enabled(),
		"buckets":      buckets,
	})
}
