// Package probe provides protocol probing for OpenAI-compatible providers.
// It discovers which protocols (chat, responses, messages) a provider supports
// natively and the correct endpoint paths (with or without /v1 prefix).
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Result holds the outcome of probing one protocol on one provider.
type Result struct {
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	Status     string `json:"status"` // "success" or "error"
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// ProviderTarget describes a provider to probe.
type ProviderTarget struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string // test model to use
}

// AllProtocols are the protocols probed by default.
var AllProtocols = []string{"chat", "responses", "messages"}

const probeTimeout = 10 * time.Second

// ProbeProvider probes all protocols for a single provider concurrently.
// Returns a map of protocol -> Result for protocols that succeeded.
func ProbeProvider(ctx context.Context, target ProviderTarget) map[string]Result {
	return ProbeProviderProtocols(ctx, target, AllProtocols)
}

// ProbeProviderProtocols probes specific protocols for a provider concurrently.
func ProbeProviderProtocols(ctx context.Context, target ProviderTarget, protocols []string) map[string]Result {
	if target.BaseURL == "" || target.Model == "" {
		return nil
	}
	client := &http.Client{Timeout: probeTimeout}
	results := make(map[string]Result, len(protocols))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, proto := range protocols {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			result := probeOneProtocol(ctx, client, target.BaseURL, target.APIKey, target.Model, p)
			if result.Status == "success" {
				mu.Lock()
				results[p] = result
				mu.Unlock()
			}
		}(proto)
	}
	wg.Wait()
	if len(results) == 0 {
		return nil
	}
	return results
}

// SupportedProtocols returns the list of protocols that were successfully probed.
func SupportedProtocols(results map[string]Result) []string {
	if len(results) == 0 {
		return nil
	}
	out := make([]string, 0, len(results))
	for _, proto := range AllProtocols {
		if r, ok := results[proto]; ok && r.Status == "success" {
			out = append(out, proto)
		}
	}
	return out
}

// ProtocolEndpoints extracts the working endpoint paths from probe results.
// Keys are protocol names, values are path suffixes relative to base URL.
func ProtocolEndpoints(baseURL string, results map[string]Result) map[string]string {
	if len(results) == 0 {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	out := make(map[string]string, len(results))
	for proto, result := range results {
		if result.Status != "success" || result.Endpoint == "" {
			continue
		}
		// Extract path suffix relative to base URL.
		endpoint := result.Endpoint
		if strings.HasPrefix(endpoint, base) {
			suffix := strings.TrimPrefix(endpoint, base)
			if suffix != "" {
				out[proto] = suffix
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- Internal probe logic (extracted from management/protocol_probe.go) ---

// Unified dual-path probe strategy:
// For every protocol, try TWO paths (OR relationship):
//
//	Path A: base + suffix         (direct append, no extra /v1)
//	Path B: base + "/v1" + suffix (with /v1 prefix)
//
// If either succeeds → protocol supported, record the working endpoint.
func probeOneProtocol(ctx context.Context, client *http.Client, base, apiKey, model, proto string) Result {
	primary, alternate := EndpointPair(base, proto)
	result := probeOneEndpoint(ctx, client, primary, apiKey, model, proto)
	if result.Status == "success" {
		return result
	}
	if alternate != "" && alternate != primary {
		altResult := probeOneEndpoint(ctx, client, alternate, apiKey, model, proto)
		if altResult.Status == "success" {
			return altResult
		}
	}
	return result
}

// EndpointPair returns the two endpoint URLs to try for probing.
// primary = base + suffix (direct append)
// alternate = base + "/v1" + suffix (with /v1 inserted)
func EndpointPair(base, proto string) (primary, alternate string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	suffix := ProtocolSuffix(proto)
	primary = base + suffix
	alternate = base + "/v1" + suffix
	if primary == alternate {
		alternate = ""
	}
	return primary, alternate
}

// ProtocolSuffix returns the path suffix for a given protocol.
func ProtocolSuffix(proto string) string {
	switch proto {
	case "responses":
		return "/responses"
	case "messages":
		return "/messages"
	default:
		return "/chat/completions"
	}
}

func probeOneEndpoint(ctx context.Context, client *http.Client, endpoint, apiKey, model, proto string) Result {
	body, headers := probePayload(proto, model, apiKey)
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{Protocol: proto, Endpoint: endpoint, Status: "error", Error: err.Error()}
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return Result{Protocol: proto, Endpoint: endpoint, Status: "error", LatencyMS: latency, Error: err.Error()}
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
		return Result{Protocol: proto, Endpoint: endpoint, Status: "error", StatusCode: resp.StatusCode, LatencyMS: latency, Error: msg}
	}
	if !json.Valid(bytes.TrimSpace(raw)) && !bytes.Contains(raw, []byte("data:")) {
		return Result{Protocol: proto, Endpoint: endpoint, Status: "error", StatusCode: resp.StatusCode, LatencyMS: latency, Error: "response is not JSON/SSE"}
	}
	return Result{Protocol: proto, Endpoint: endpoint, Status: "success", StatusCode: resp.StatusCode, LatencyMS: latency}
}

func probePayload(proto, model, apiKey string) ([]byte, map[string]string) {
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
