package management

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

type protocolProbeBatchRequest struct {
	ProviderName string   `json:"provider_name"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	Protocols    []string `json:"protocols"`
	AutoApply    bool     `json:"auto_apply"`
	TimeoutMS    int      `json:"timeout_ms"`
	// KeyIndex optionally scopes the probe to one API key entry: its key is
	// used for requests and its per-key model list (falling back to the
	// provider-level models when the entry has none) is probed.
	KeyIndex *int `json:"key_index,omitempty"`
}

type protocolProbeBatchModelResult struct {
	Model     string            `json:"model"`
	Protocols map[string]string `json:"protocols"`
	Endpoints map[string]string `json:"endpoints,omitempty"`
}

// ProtocolProbeBatch probes all (or specified) models in an openai-compatibility
// provider to detect which protocols each model supports. Optionally writes back
// the detected protocols to per-model config.
func (h *Handler) ProtocolProbeBatch(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "handler unavailable"})
		return
	}

	var req protocolProbeBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	providerName := strings.TrimSpace(req.ProviderName)
	if providerName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider_name is required"})
		return
	}
	reqBaseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")

	// Locate the openai-compatibility config entry. Probing is a diagnostic
	// action, so disabled entries are still eligible. Prefer an exact
	// name+base match, but fall back to name-only so unsaved base URL edits
	// in the form do not break the lookup.
	compatIdx := -1
	nameOnlyIdx := -1
	for i := range h.cfg.OpenAICompatibility {
		compat := &h.cfg.OpenAICompatibility[i]
		name := strings.TrimSpace(compat.Name)
		if !strings.EqualFold(name, providerName) {
			continue
		}
		if nameOnlyIdx < 0 {
			nameOnlyIdx = i
		}
		if reqBaseURL != "" {
			entryBase := strings.TrimRight(strings.TrimSpace(compat.BaseURL), "/")
			if !strings.EqualFold(entryBase, reqBaseURL) {
				continue
			}
		}
		compatIdx = i
		break
	}
	if compatIdx < 0 {
		compatIdx = nameOnlyIdx
	}
	if compatIdx < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}

	compat := &h.cfg.OpenAICompatibility[compatIdx]

	// Resolve the selected key entry when key_index is provided.
	keyIdx := -1
	if req.KeyIndex != nil {
		keyIdx = *req.KeyIndex
		if keyIdx < 0 || keyIdx >= len(compat.APIKeyEntries) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key_index out of range"})
			return
		}
	}

	// Determine models to probe. With a selected key entry, probe its per-key
	// model list (falling back to the provider defaults when empty). Without
	// one, collect from the provider level and every key entry.
	var models []string
	if len(req.Models) > 0 {
		models = req.Models
	} else {
		seen := make(map[string]struct{})
		appendModel := func(name string) {
			name = strings.TrimSpace(name)
			if name == "" {
				return
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			models = append(models, name)
		}
		if keyIdx >= 0 {
			for _, m := range compat.APIKeyEntries[keyIdx].Models {
				appendModel(m.Name)
			}
			if len(models) == 0 {
				for _, m := range compat.Models {
					appendModel(m.Name)
				}
			}
		} else {
			for _, m := range compat.Models {
				appendModel(m.Name)
			}
			for i := range compat.APIKeyEntries {
				for _, m := range compat.APIKeyEntries[i].Models {
					appendModel(m.Name)
				}
			}
		}
	}
	if len(models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no models to probe"})
		return
	}

	// Determine protocols to probe.
	wantProtocols := []string{"chat", "responses", "messages"}
	if len(req.Protocols) > 0 {
		wantProtocols = util.NormalizeUpstreamProtocols(req.Protocols)
	}

	// Resolve API key: the selected entry when key_index is provided,
	// otherwise the first available key entry.
	apiKey := ""
	if keyIdx >= 0 {
		apiKey = strings.TrimSpace(compat.APIKeyEntries[keyIdx].APIKey)
	} else if len(compat.APIKeyEntries) > 0 {
		apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(compat.BaseURL), "/")

	timeout := 30 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > 90*time.Second {
		timeout = 90 * time.Second
	}

	client := &http.Client{Timeout: timeout}

	// Probe all models in parallel.
	results := make([]protocolProbeBatchModelResult, len(models))
	var wg sync.WaitGroup
	for i, model := range models {
		wg.Add(1)
		go func(idx int, modelName string) {
			defer wg.Done()
			protos := make(map[string]string, len(wantProtocols))
			endpoints := make(map[string]string, len(wantProtocols))
			for _, proto := range wantProtocols {
				r := probeOneProtocol(c.Request.Context(), client, baseURL, apiKey, modelName, proto)
				protos[proto] = r.Status
				if r.Status == "success" && r.Endpoint != "" {
					// Extract path suffix relative to base URL for storage.
					suffix := extractEndpointSuffix(baseURL, r.Endpoint)
					if suffix != "" {
						endpoints[proto] = suffix
					}
				}
			}
			results[idx] = protocolProbeBatchModelResult{
				Model:     modelName,
				Protocols: protos,
				Endpoints: endpoints,
			}
		}(i, model)
	}
	wg.Wait()

	// Optionally apply detected protocols back to config.
	applied := false
	if req.AutoApply {
		applied = h.applyBatchProbeResults(c, compatIdx, results)
	}

	c.JSON(http.StatusOK, gin.H{
		"results": results,
		"applied": applied,
	})
}

// applyBatchProbeResults writes detected per-model protocols back to config.
func (h *Handler) applyBatchProbeResults(c *gin.Context, compatIdx int, results []protocolProbeBatchModelResult) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if compatIdx < 0 || compatIdx >= len(h.cfg.OpenAICompatibility) {
		return false
	}
	compat := &h.cfg.OpenAICompatibility[compatIdx]

	changed := false
	for _, r := range results {
		// Collect successful protocols for this model.
		var supported []string
		for _, proto := range []string{"responses", "chat", "messages"} {
			if status, ok := r.Protocols[proto]; ok && status == "success" {
				supported = append(supported, proto)
			}
		}
		if len(supported) == 0 {
			continue
		}

		// Update the model wherever it is defined: provider-level list and
		// any per-key model lists.
		applyToModel := func(m *config.OpenAICompatibilityModel) {
			normalized := util.NormalizeUpstreamProtocols(supported)
			if !protocolSlicesEqual(m.Protocols, normalized) {
				m.Protocols = normalized
				changed = true
				log.Infof("protocol-probe-batch: updated model %s protocols to %v", r.Model, normalized)
			}
			// Save discovered endpoint paths.
			if len(r.Endpoints) > 0 {
				if m.ProtocolEndpoints == nil {
					m.ProtocolEndpoints = make(map[string]string)
				}
				for proto, suffix := range r.Endpoints {
					if m.ProtocolEndpoints[proto] != suffix {
						m.ProtocolEndpoints[proto] = suffix
						changed = true
						log.Infof("protocol-probe-batch: stored endpoint for model %s protocol %s: %s", r.Model, proto, suffix)
					}
				}
			}
		}
		for j := range compat.Models {
			if strings.EqualFold(strings.TrimSpace(compat.Models[j].Name), strings.TrimSpace(r.Model)) {
				applyToModel(&compat.Models[j])
			}
		}
		for k := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[k]
			for j := range entry.Models {
				if strings.EqualFold(strings.TrimSpace(entry.Models[j].Name), strings.TrimSpace(r.Model)) {
					applyToModel(&entry.Models[j])
				}
			}
		}
	}

	if !changed {
		return true
	}

	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		log.Errorf("protocol-probe-batch: failed to save config: %v", err)
		return false
	}
	// Reload config into handler.
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		log.Errorf("protocol-probe-batch: failed to reload config after save: %v", err)
		return true // saved but reload failed
	}
	h.cfg = newCfg
	return true
}

func protocolSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[strings.ToLower(strings.TrimSpace(v))] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[strings.ToLower(strings.TrimSpace(v))]; !ok {
			return false
		}
	}
	return true
}

// extractEndpointSuffix derives the path suffix from a full endpoint URL
// relative to the base URL. For example, given base "https://x.com/api/v3"
// and endpoint "https://x.com/api/v3/responses", it returns "/responses".
func extractEndpointSuffix(base, endpoint string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	endpoint = strings.TrimSpace(endpoint)
	if strings.HasPrefix(endpoint, base) {
		suffix := endpoint[len(base):]
		if suffix == "" {
			return ""
		}
		return suffix
	}
	return ""
}
