package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/requestjournal"
)

func TestParseUserSideWindow(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{raw: "", want: 24 * time.Hour},
		{raw: "15m", want: 15 * time.Minute},
		{raw: "1h", want: time.Hour},
		{raw: "7d", want: 7 * 24 * time.Hour},
		{raw: "45d", want: 30 * 24 * time.Hour}, // capped at journal retention
		{raw: "garbage", want: 24 * time.Hour},
	}
	for _, test := range tests {
		if got := parseUserSideWindow(test.raw); got != test.want {
			t.Fatalf("parseUserSideWindow(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

func TestBuildUserSideAvailabilityEdges(t *testing.T) {
	agg := map[requestjournal.CapabilityEdgeKey]requestjournal.UserSideAvailability{
		{Provider: "p", AuthIndex: "a", Model: "m", Protocol: "http"}: {Resolved: 8, HardFailed: 2},
		{Provider: "p", AuthIndex: "b", Model: "m", Protocol: "http"}: {Resolved: 5, HardFailed: 0},
		{Provider: "q", AuthIndex: "a", Model: "n", Protocol: "sse"}:  {Resolved: 0, HardFailed: 3},
	}

	edges, resolved, hardFailed := buildUserSideAvailabilityEdges(agg, requestjournal.UserSideFilters{})
	if resolved != 13 || hardFailed != 5 {
		t.Fatalf("totals = %d resolved / %d hard-failed, want 13/5", resolved, hardFailed)
	}
	// Hard-failed first, then provider/model/auth.
	if len(edges) != 3 {
		t.Fatalf("edge count = %d, want 3", len(edges))
	}
	if edges[0].Provider != "q" || edges[0].HardFailed != 3 {
		t.Fatalf("first edge = %+v, want q hard-failed 3 first", edges[0])
	}
	if edges[1].Provider != "p" || edges[1].AuthIndex != "a" {
		t.Fatalf("second edge = %+v, want p/a", edges[1])
	}
	if edges[2].Provider != "p" || edges[2].AuthIndex != "b" {
		t.Fatalf("third edge = %+v, want p/b", edges[2])
	}
	// Availability is resolved/total.
	if edges[1].Total != 10 || edges[1].Availability != 0.8 {
		t.Fatalf("p/a edge = %+v, want total 10, availability 0.8", edges[1])
	}
	if edges[2].Total != 5 || edges[2].Availability != 1.0 {
		t.Fatalf("p/b edge = %+v, want total 5, availability 1.0", edges[2])
	}
	if edges[0].Availability != 0.0 {
		t.Fatalf("q/a edge availability = %v, want 0.0", edges[0].Availability)
	}
}

func TestBuildUserSideAvailabilityEdgesFilters(t *testing.T) {
	agg := map[requestjournal.CapabilityEdgeKey]requestjournal.UserSideAvailability{
		{Provider: "p", AuthIndex: "a", Model: "m", Protocol: "http"}: {Resolved: 1},
		{Provider: "q", AuthIndex: "a", Model: "n", Protocol: "sse"}:  {Resolved: 1},
	}
	edges, _, _ := buildUserSideAvailabilityEdges(agg, requestjournal.UserSideFilters{Provider: "p"})
	if len(edges) != 1 || edges[0].Provider != "p" {
		t.Fatalf("provider filter: got %+v, want only p", edges)
	}
}

func TestGetUserSideAvailabilityReturnsEmptyWithoutJournal(t *testing.T) {
	// Point the journal at a throwaway dir so the endpoint exercises the real
	// read path and returns a well-formed, empty response. Reset to disabled
	// afterwards; no other management test configures the journal.
	requestjournal.Configure(t.TempDir(), 30)
	t.Cleanup(func() { requestjournal.Configure("", 0) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.GET("/user-side-availability", h.GetUserSideAvailability)

	req := httptest.NewRequest(http.MethodGet, "/user-side-availability", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Window        string         `json:"window"`
		Overall       float64        `json:"overall_availability"`
		TotalRequests int64          `json:"total_requests"`
		Durable       bool           `json:"durable"`
		Edges         []userSideEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Window != (24 * time.Hour).String() {
		t.Fatalf("window = %q, want 24h0m0s", resp.Window)
	}
	if resp.Overall != 0 || resp.TotalRequests != 0 || len(resp.Edges) != 0 {
		t.Fatalf("expected empty result, got %+v", resp)
	}
}

func TestGetUserSideRequestChainsReturnsEmptyWithoutJournal(t *testing.T) {
	requestjournal.Configure(t.TempDir(), 30)
	t.Cleanup(func() { requestjournal.Configure("", 0) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.GET("/user-side-availability/requests", h.GetUserSideRequestChains)

	req := httptest.NewRequest(http.MethodGet, "/user-side-availability/requests", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Window string                        `json:"window"`
		Count  int                           `json:"count"`
		Chains []requestjournal.RequestChain `json:"chains"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Window != (24 * time.Hour).String() {
		t.Fatalf("window = %q, want 24h0m0s", resp.Window)
	}
	if resp.Count != 0 || len(resp.Chains) != 0 {
		t.Fatalf("expected empty result, got count=%d chains=%d", resp.Count, len(resp.Chains))
	}
}

func TestGetUserSideAvailabilityTrendReturnsEmptyWithoutJournal(t *testing.T) {
	requestjournal.Configure(t.TempDir(), 30)
	t.Cleanup(func() { requestjournal.Configure("", 0) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.GET("/user-side-availability/trend", h.GetUserSideAvailabilityTrend)

	req := httptest.NewRequest(http.MethodGet, "/user-side-availability/trend?window=1h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Window  string                       `json:"window"`
		Buckets []requestjournal.TrendBucket `json:"buckets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Window != time.Hour.String() {
		t.Fatalf("window = %q, want %q", resp.Window, time.Hour.String())
	}
	if len(resp.Buckets) != 12 { // 1h / 5m buckets, all empty
		t.Fatalf("bucket count = %d, want 12", len(resp.Buckets))
	}
}
