package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestProtocolProbeBatch_MissingProviderName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{},
	}

	body, _ := json.Marshal(protocolProbeBatchRequest{})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/protocol-probe-batch", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ProtocolProbeBatch(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "provider_name is required" {
		t.Fatalf("unexpected error: %s", resp["error"])
	}
}

func TestProtocolProbeBatch_ProviderNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "test-provider", BaseURL: "https://example.com/v1"},
			},
		},
	}

	body, _ := json.Marshal(protocolProbeBatchRequest{
		ProviderName: "nonexistent",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/protocol-probe-batch", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ProtocolProbeBatch(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProtocolProbeBatch_NoModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "empty-provider", BaseURL: "https://example.com/v1", Models: nil},
			},
		},
	}

	body, _ := json.Marshal(protocolProbeBatchRequest{
		ProviderName: "empty-provider",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/protocol-probe-batch", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ProtocolProbeBatch(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProtocolProbeBatch_ProbesModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Start a fake upstream that accepts chat but rejects responses/messages.
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not supported"}`))
		}
	}))
	defer fakeUpstream.Close()

	h := &Handler{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "test-provider",
					BaseURL: fakeUpstream.URL,
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "sk-test"},
					},
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
				},
			},
		},
	}

	body, _ := json.Marshal(protocolProbeBatchRequest{
		ProviderName: "test-provider",
		Protocols:    []string{"chat", "responses"},
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/protocol-probe-batch", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ProtocolProbeBatch(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []protocolProbeBatchModelResult `json:"results"`
		Applied bool                            `json:"applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	for _, r := range resp.Results {
		if r.Protocols["chat"] != "success" {
			t.Errorf("model %s: expected chat=success, got %s", r.Model, r.Protocols["chat"])
		}
		if r.Protocols["responses"] != "error" {
			t.Errorf("model %s: expected responses=error, got %s", r.Model, r.Protocols["responses"])
		}
	}
}

func TestProtocolProbeBatch_SlowPathDoesNotBlockSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Fake upstream: chat answers instantly, responses hangs until the
	// client deadline. Concurrent probing must still report chat success
	// and finish quickly instead of serially waiting on the slow path.
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		default:
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			w.WriteHeader(http.StatusRequestTimeout)
		}
	}))
	defer fakeUpstream.Close()

	h := &Handler{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "test-provider",
					BaseURL: fakeUpstream.URL,
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "sk-test"},
					},
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
					},
				},
			},
		},
	}

	body, _ := json.Marshal(protocolProbeBatchRequest{
		ProviderName: "test-provider",
		Protocols:    []string{"chat", "responses"},
		TimeoutMS:    800,
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/protocol-probe-batch", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	h.ProtocolProbeBatch(c)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// With parallel probing the whole batch must finish shortly after the
	// per-request timeout, well below serial worst case.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("batch probe took too long: %v", elapsed)
	}

	var resp struct {
		Results []protocolProbeBatchModelResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].Protocols["chat"] != "success" {
		t.Errorf("expected chat=success, got %s", resp.Results[0].Protocols["chat"])
	}
	if resp.Results[0].Protocols["responses"] == "success" {
		t.Errorf("expected responses to fail, got %s", resp.Results[0].Protocols["responses"])
	}
}

func TestProtocolSlicesEqual(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"chat"}, []string{"chat"}, true},
		{[]string{"chat", "responses"}, []string{"responses", "chat"}, true},
		{[]string{"chat"}, []string{"responses"}, false},
		{[]string{"chat", "responses"}, []string{"chat"}, false},
	}
	for _, tt := range tests {
		got := protocolSlicesEqual(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("protocolSlicesEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
