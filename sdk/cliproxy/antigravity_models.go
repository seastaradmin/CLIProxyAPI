package cliproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelBaseURLDaily = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd  = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath        = "/v1internal:fetchAvailableModels"
)

type antigravityFetchAvailableModelsResponse struct {
	Models            map[string]antigravityAvailableModel `json:"models"`
	WebSearchModelIDs []string                             `json:"webSearchModelIds"`
}

type antigravityAvailableModel struct {
	DisplayName         string `json:"displayName"`
	MaxTokens           int    `json:"maxTokens"`
	MaxCompletionTokens int    `json:"maxOutputTokens"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
}

type antigravityFetchedModels struct {
	Models            []*ModelInfo
	CapabilityHints   antigravityModelCapabilityHints
	AuthoritativeList bool
}

func (s *Service) fetchAntigravityModelsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityFetchedModels {
	if auth == nil || auth.Metadata == nil {
		return antigravityFetchedModels{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityFetchedModels{}
	}

	client := &http.Client{}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth)); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	payload := antigravityModelsRequestPayload(auth)
	for _, baseURL := range antigravityModelBaseURLs(auth) {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(string(payload)))
		if errReq != nil {
			continue
		}
		req.Close = true
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())

		resp, errDo := client.Do(req)
		if errDo != nil {
			continue
		}
		body, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
		if errRead != nil {
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			continue
		}
		fetched, ok := parseAntigravityFetchedModels(body)
		if ok {
			return fetched
		}
	}
	return antigravityFetchedModels{}
}

func antigravityModelsRequestPayload(auth *coreauth.Auth) []byte {
	if auth == nil || auth.Metadata == nil {
		return []byte(`{}`)
	}
	projectID, _ := auth.Metadata["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return []byte(`{}`)
	}
	payload, errMarshal := json.Marshal(struct {
		Project string `json:"project"`
	}{Project: projectID})
	if errMarshal != nil {
		return []byte(`{}`)
	}
	return payload
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil && s.cfg != nil {
		return strings.TrimSpace(s.cfg.ProxyURL)
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily, antigravityModelBaseURLProd}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func parseAntigravityFetchedModels(body []byte) (antigravityFetchedModels, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityFetchedModels{}, false
	}
	if parsed.Models == nil {
		return antigravityFetchedModels{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}

	modelIDs := make([]string, 0, len(parsed.Models))
	for modelID := range parsed.Models {
		modelID = strings.TrimSpace(modelID)
		if modelID != "" {
			modelIDs = append(modelIDs, modelID)
		}
	}
	sort.Strings(modelIDs)
	models := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model := parsed.Models[modelID]
		displayName := strings.TrimSpace(model.DisplayName)
		if displayName == "" {
			displayName = modelID
		}
		models = append(models, &ModelInfo{
			ID:                  modelID,
			Object:              "model",
			OwnedBy:             "antigravity",
			Type:                "antigravity",
			DisplayName:         displayName,
			Name:                modelID,
			Description:         displayName,
			ContextLength:       model.MaxTokens,
			MaxCompletionTokens: model.MaxCompletionTokens,
		})
	}

	return antigravityFetchedModels{
		Models:            models,
		CapabilityHints:   antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels},
		AuthoritativeList: true,
	}, true
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	if len(models) == 0 {
		return models
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		_, model.SupportsWebSearch = hints.WebSearchModelIDs[modelID]
	}
	return models
}

// mergeAntigravityFetchedModels preserves static metadata for models that the
// upstream account currently exposes. The upstream response remains authoritative
// for membership and current display/token metadata.
func mergeAntigravityFetchedModels(staticModels, fetchedModels []*ModelInfo) []*ModelInfo {
	staticByID := make(map[string]*ModelInfo, len(staticModels))
	for _, model := range staticModels {
		if model == nil {
			continue
		}
		if modelID := normalizeAntigravityFetchedModelID(model.ID); modelID != "" {
			staticByID[modelID] = model
		}
	}

	models := make([]*ModelInfo, 0, len(fetchedModels))
	for _, fetched := range fetchedModels {
		if fetched == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(fetched.ID)
		if modelID == "" {
			continue
		}
		static := staticByID[modelID]
		if static == nil {
			model := *fetched
			models = append(models, &model)
			continue
		}

		model := *static
		model.ID = fetched.ID
		model.Object = fetched.Object
		model.OwnedBy = fetched.OwnedBy
		model.Type = fetched.Type
		model.Name = fetched.Name
		if fetched.DisplayName != "" {
			model.DisplayName = fetched.DisplayName
		}
		if fetched.Description != "" {
			model.Description = fetched.Description
		}
		if fetched.ContextLength > 0 {
			model.ContextLength = fetched.ContextLength
		}
		if fetched.MaxCompletionTokens > 0 {
			model.MaxCompletionTokens = fetched.MaxCompletionTokens
		}
		models = append(models, &model)
	}
	return models
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}
