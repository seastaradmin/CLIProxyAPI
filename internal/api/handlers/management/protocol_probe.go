package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

type protocolProbeRequest struct {
	BaseURL   string   `json:"base_url"`
	APIKey    string   `json:"api_key"`
	Model     string   `json:"model"`
	Protocols []string `json:"protocols"`
	TimeoutMS int      `json:"timeout_ms"`
}

type protocolProbeResult struct {
	Protocol    string `json:"protocol"`
	Endpoint    string `json:"endpoint"`
	Status      string `json:"status"`
	StatusCode  int    `json:"status_code,omitempty"`
	LatencyMS   int64  `json:"latency_ms"`
	Error       string `json:"error,omitempty"`
	Passthrough bool   `json:"passthrough"`
}

// ProbeUpstreamProtocols tests chat/responses/messages against a base URL.
func (h *Handler) ProbeUpstreamProtocols(c *gin.Context) {
	var req protocolProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "base_url is required"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	apiKey := strings.TrimSpace(req.APIKey)
	timeout := 30 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}

	want := util.NormalizeUpstreamProtocols(req.Protocols)
	if len(req.Protocols) == 0 {
		want = []string{"chat", "responses", "messages"}
	}

	client := &http.Client{Timeout: timeout}
	results := make([]protocolProbeResult, 0, len(want))
	for _, proto := range want {
		results = append(results, probeOneProtocol(c.Request.Context(), client, base, apiKey, model, proto))
	}

	supported := make([]string, 0, 3)
	for _, r := range results {
		if r.Status == "success" {
			supported = append(supported, r.Protocol)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"base_url":            base,
		"model":               model,
		"supported":           util.NormalizeUpstreamProtocols(supported),
		"results":             results,
		"suggested_protocols": util.NormalizeUpstreamProtocols(supported),
	})
}

// Unified dual-path probe strategy:
// For every protocol, try TWO paths (OR relationship):
//   Path A: base + suffix         (direct append, no extra /v1)
//   Path B: base + "/v1" + suffix (with /v1 prefix)
// If either succeeds → protocol supported, record the working endpoint.
//
// Protocol suffix table:
//   chat      → /chat/completions
//   responses → /responses
//   messages  → /messages

func probeOneProtocol(ctx context.Context, client *http.Client, base, apiKey, model, proto string) protocolProbeResult {
	primary, alternate := protocolEndpointPair(base, proto)
	result := probeOneProtocolEndpoint(ctx, client, primary, apiKey, model, proto)
	if result.Status == "success" {
		return result
	}
	if alternate != "" && alternate != primary {
		altResult := probeOneProtocolEndpoint(ctx, client, alternate, apiKey, model, proto)
		if altResult.Status == "success" {
			return altResult
		}
	}
	return result
}

// protocolSuffix returns the path suffix for a given protocol.
func protocolSuffix(proto string) string {
	switch proto {
	case "responses":
		return "/responses"
	case "messages":
		return "/messages"
	default:
		return "/chat/completions"
	}
}

// protocolEndpointPair returns the two endpoint URLs to try for probing.
// primary = base + suffix (direct append)
// alternate = base + "/v1" + suffix (with /v1 inserted)
func protocolEndpointPair(base, proto string) (primary, alternate string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	suffix := protocolSuffix(proto)
	primary = base + suffix
	alternate = base + "/v1" + suffix
	// If they're the same (e.g. base already ends with /v1 and suffix starts the same way), skip alternate.
	if primary == alternate {
		alternate = ""
	}
	return primary, alternate
}

func probeOneProtocolEndpoint(ctx context.Context, client *http.Client, endpoint, apiKey, model, proto string) protocolProbeResult {
	body, headers := protocolProbePayload(proto, model, apiKey)
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return protocolProbeResult{Protocol: proto, Endpoint: endpoint, Status: "error", Error: err.Error(), Passthrough: true}
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return protocolProbeResult{Protocol: proto, Endpoint: endpoint, Status: "error", LatencyMS: latency, Error: err.Error(), Passthrough: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return protocolProbeResult{
			Protocol:    proto,
			Endpoint:    endpoint,
			Status:      "error",
			StatusCode:  resp.StatusCode,
			LatencyMS:   latency,
			Error:       msg,
			Passthrough: true,
		}
	}
	if !json.Valid(bytes.TrimSpace(raw)) && !bytes.Contains(raw, []byte("data:")) {
		return protocolProbeResult{
			Protocol:    proto,
			Endpoint:    endpoint,
			Status:      "error",
			StatusCode:  resp.StatusCode,
			LatencyMS:   latency,
			Error:       "response is not JSON/SSE",
			Passthrough: true,
		}
	}
	return protocolProbeResult{
		Protocol:    proto,
		Endpoint:    endpoint,
		Status:      "success",
		StatusCode:  resp.StatusCode,
		LatencyMS:   latency,
		Passthrough: true,
	}
}

func protocolProbePayload(proto, model, apiKey string) ([]byte, map[string]string) {
	headers := map[string]string{"Content-Type": "application/json"}
	switch proto {
	case "responses":
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
		body, _ := json.Marshal(map[string]any{
			"model": model, "input": "Reply with exactly: OK", "stream": false,
		})
		return body, headers
	case "messages":
		if apiKey != "" {
			headers["x-api-key"] = apiKey
			headers["Authorization"] = "Bearer " + apiKey
		}
		headers["anthropic-version"] = "2023-06-01"
		body, _ := json.Marshal(map[string]any{
			"model": model, "max_tokens": 16, "stream": false,
			"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: OK"}},
		})
		return body, headers
	default:
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
		body, _ := json.Marshal(map[string]any{
			"model": model, "max_tokens": 16, "stream": false,
			"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: OK"}},
		})
		return body, headers
	}
}
