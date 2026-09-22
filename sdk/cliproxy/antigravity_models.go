package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityModelBaseURLDaily      = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd       = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath             = "/v1internal:fetchAvailableModels"
	antigravityCapabilityProbeTimeout = 5 * time.Second
	antigravityCapabilityCacheTTL     = 5 * time.Minute
	antigravityCapabilityFailureTTL   = 1 * time.Minute
)

type antigravityProbeStatus int

const (
	antigravityProbeStatusSuccess antigravityProbeStatus = iota
	antigravityProbeStatusAuthError
	antigravityProbeStatusTransientError
)

type antigravityCapabilityCacheEntry struct {
	hints     antigravityModelCapabilityHints
	expiresAt time.Time
}

type antigravityProbeResult struct {
	hints  antigravityModelCapabilityHints
	status antigravityProbeStatus
	token  string
}

var (
	antigravityNowFunc          = time.Now
	antigravityCapabilityMu     sync.RWMutex
	antigravityCapabilityCache  = make(map[string]antigravityCapabilityCacheEntry)
	antigravityAuthFailureCache = make(map[string]time.Time)
	antigravityCapabilityGroup  singleflight.Group
	antigravityProbeInFlight    atomic.Int32
)

func purgeExpiredAntigravityCacheLocked(now time.Time) {
	for k, v := range antigravityCapabilityCache {
		if !now.Before(v.expiresAt) {
			delete(antigravityCapabilityCache, k)
		}
	}
	for k, exp := range antigravityAuthFailureCache {
		if !now.Before(exp) {
			delete(antigravityAuthFailureCache, k)
		}
	}
}

type antigravityFetchAvailableModelsResponse struct {
	Models            map[string]antigravityAvailableModel `json:"models"`
	WebSearchModelIDs []string                             `json:"webSearchModelIds"`
}

// antigravityAvailableModel is one entry of the upstream model catalog.
type antigravityAvailableModel struct {
	DisplayName         string `json:"displayName"`
	MaxTokens           int    `json:"maxTokens"`
	MaxCompletionTokens int    `json:"maxOutputTokens"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
}

// antigravityFetchedModels carries the authoritative model catalog reported by
// the upstream fetchAvailableModels endpoint, alongside the capability hints
// parsed from the same response.
type antigravityFetchedModels struct {
	Models            []*ModelInfo
	CapabilityHints   antigravityModelCapabilityHints
	AuthoritativeList bool
}

func (h antigravityModelCapabilityHints) clone() antigravityModelCapabilityHints {
	if h.WebSearchModelIDs == nil {
		return antigravityModelCapabilityHints{}
	}
	cloned := make(map[string]struct{}, len(h.WebSearchModelIDs))
	for k := range h.WebSearchModelIDs {
		cloned[k] = struct{}{}
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: cloned}
}

func (s *Service) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil || auth.Metadata == nil {
		return antigravityModelCapabilityHints{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityModelCapabilityHints{}
	}

	baseURLs := antigravityModelBaseURLs(auth)
	if len(baseURLs) == 0 {
		return antigravityModelCapabilityHints{}
	}

	proxyURL := s.antigravityModelFetchProxyURL(auth)
	cacheKey := strings.Join(baseURLs, "|") + "#" + proxyURL
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(accessToken)))
	authFailKey := auth.ID + "#" + tokenHash

	now := antigravityNowFunc()
	antigravityCapabilityMu.RLock()
	if failExpiry, failed := antigravityAuthFailureCache[authFailKey]; failed && now.Before(failExpiry) {
		antigravityCapabilityMu.RUnlock()
		return antigravityModelCapabilityHints{}
	}
	if entry, ok := antigravityCapabilityCache[cacheKey]; ok && now.Before(entry.expiresAt) {
		hints := entry.hints.clone()
		antigravityCapabilityMu.RUnlock()
		return hints
	}
	antigravityCapabilityMu.RUnlock()

	antigravityProbeInFlight.Add(1)
	defer antigravityProbeInFlight.Add(-1)

	res, errDo, _ := antigravityCapabilityGroup.Do(cacheKey, func() (any, error) {
		nowInside := antigravityNowFunc()
		antigravityCapabilityMu.RLock()
		if entry, ok := antigravityCapabilityCache[cacheKey]; ok && nowInside.Before(entry.expiresAt) {
			hints := entry.hints.clone()
			antigravityCapabilityMu.RUnlock()
			return antigravityProbeResult{hints: hints, status: antigravityProbeStatusSuccess, token: accessToken}, nil
		}
		antigravityCapabilityMu.RUnlock()

		hints, status := s.probeAntigravityModelCapabilityHints(ctx, auth, baseURLs, proxyURL, accessToken)
		if status == antigravityProbeStatusSuccess || status == antigravityProbeStatusTransientError {
			if status != antigravityProbeStatusTransientError || ctx == nil || ctx.Err() == nil {
				ttl := antigravityCapabilityCacheTTL
				if status != antigravityProbeStatusSuccess {
					ttl = antigravityCapabilityFailureTTL
				}
				antigravityCapabilityMu.Lock()
				purgeExpiredAntigravityCacheLocked(nowInside)
				antigravityCapabilityCache[cacheKey] = antigravityCapabilityCacheEntry{
					hints:     hints.clone(),
					expiresAt: antigravityNowFunc().Add(ttl),
				}
				antigravityCapabilityMu.Unlock()
			}
		}
		return antigravityProbeResult{hints: hints, status: status, token: accessToken}, nil
	})
	if errDo != nil || res == nil {
		return antigravityModelCapabilityHints{}
	}
	result, ok := res.(antigravityProbeResult)
	if !ok {
		return antigravityModelCapabilityHints{}
	}

	if result.status == antigravityProbeStatusAuthError {
		if result.token != accessToken {
			// The shared probe failed with an auth error from another account's token.
			// Re-try with our own account's token.
			return s.fetchAntigravityModelCapabilityHintsForAuth(ctx, auth)
		}
		// Our own token failed with 401/403. Record backoff for this account/token.
		antigravityCapabilityMu.Lock()
		purgeExpiredAntigravityCacheLocked(now)
		antigravityAuthFailureCache[authFailKey] = antigravityNowFunc().Add(antigravityCapabilityFailureTTL)
		antigravityCapabilityMu.Unlock()
		return antigravityModelCapabilityHints{}
	}

	return result.hints.clone()
}

func (s *Service) probeAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL string, accessToken string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	probeCtx := context.Background()
	var cancel context.CancelFunc
	probeCtx, cancel = context.WithTimeout(probeCtx, antigravityCapabilityProbeTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: antigravityCapabilityProbeTimeout,
	}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyURL); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	if len(baseURLs) == 1 {
		return s.fetchAntigravityModelHintsFromURL(probeCtx, client, baseURLs[0], accessToken)
	}

	type probeResult struct {
		hints  antigravityModelCapabilityHints
		status antigravityProbeStatus
	}
	ch := make(chan probeResult, len(baseURLs))
	for _, baseURL := range baseURLs {
		go func(url string) {
			h, status := s.fetchAntigravityModelHintsFromURL(probeCtx, client, url, accessToken)
			ch <- probeResult{hints: h, status: status}
		}(baseURL)
	}

	var firstSuccess antigravityModelCapabilityHints
	var hadSuccess bool
	overallStatus := antigravityProbeStatusTransientError
	for i := 0; i < len(baseURLs); i++ {
		select {
		case <-probeCtx.Done():
			return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
		case res := <-ch:
			if res.status == antigravityProbeStatusSuccess {
				if len(res.hints.WebSearchModelIDs) > 0 {
					return res.hints, antigravityProbeStatusSuccess
				}
				if !hadSuccess {
					firstSuccess = res.hints
					hadSuccess = true
				}
			} else if res.status == antigravityProbeStatusAuthError {
				overallStatus = antigravityProbeStatusAuthError
			}
		}
	}
	if hadSuccess {
		return firstSuccess, antigravityProbeStatusSuccess
	}
	return antigravityModelCapabilityHints{}, overallStatus
}

func (s *Service) fetchAntigravityModelHintsFromURL(ctx context.Context, client *http.Client, baseURL string, accessToken string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(`{}`))
	if errReq != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	req.Close = true
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())

	resp, errDo := client.Do(req)
	if errDo != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusAuthError
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	hints, ok := parseAntigravityModelCapabilityHints(body)
	if !ok {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	return hints, antigravityProbeStatusSuccess
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil {
		s.cfgMu.RLock()
		defer s.cfgMu.RUnlock()
		if s.cfg != nil {
			return strings.TrimSpace(s.cfg.ProxyURL)
		}
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["base_urls"]); raw != "" {
			parts := strings.Split(raw, ",")
			urls := make([]string, 0, len(parts))
			for _, p := range parts {
				if trimmed := strings.TrimRight(strings.TrimSpace(p), "/"); trimmed != "" {
					urls = append(urls, trimmed)
				}
			}
			if len(urls) > 0 {
				return urls
			}
		}
	}
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily}
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

func parseAntigravityModelCapabilityHints(body []byte) (antigravityModelCapabilityHints, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels}, true
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	if len(models) == 0 || len(hints.WebSearchModelIDs) == 0 {
		return models
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; ok {
			model.SupportsWebSearch = true
		}
	}
	return models
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// fetchAntigravityModelsForAuth reads the model catalog for one credential. A response
// carrying a model map is authoritative (AuthoritativeList): it replaces the static model
// list entirely, so models that upstream no longer serves stop being advertised. A
// response without a model map is not authoritative, but it still contributes the web
// search capability hints parsed from the same payload, so one request serves both the
// catalog and the capability probe.
//
// The request is bounded by antigravityCapabilityProbeTimeout: the caller runs off the
// registration goroutine and a stalled upstream must never pin the probe wait group.
func (s *Service) fetchAntigravityModelsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityFetchedModels {
	if auth == nil || auth.Metadata == nil {
		return antigravityFetchedModels{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityFetchedModels{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, antigravityCapabilityProbeTimeout)
	defer cancel()

	client := &http.Client{}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth)); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	payload := antigravityModelsRequestPayload(auth)
	for _, baseURL := range antigravityModelBaseURLs(auth) {
		req, errReq := http.NewRequestWithContext(fetchCtx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(string(payload)))
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

// antigravityModelsRequestPayload scopes the catalog request to the credential's
// project when one is known.
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

// parseAntigravityFetchedModels decodes one fetchAvailableModels response into the
// authoritative catalog (present only when the payload carries a model map) plus the web
// search model ids reported alongside it. ok is false when the payload is not valid JSON;
// a valid response without a model map yields hints and no catalog, which leaves the
// baseline model list in place.
func parseAntigravityFetchedModels(body []byte) (antigravityFetchedModels, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityFetchedModels{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	hints := antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels}
	if parsed.Models == nil {
		return antigravityFetchedModels{CapabilityHints: hints}, true
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
		CapabilityHints:   hints,
		AuthoritativeList: true,
	}, true
}

// WaitAntigravityProbes waits for any in-flight asynchronous Antigravity probe work to
// complete. That includes the authoritative model catalog fetch that replaces a
// credential's baseline model list, so a caller that needs the final registration to be
// in place must wait here.
func (s *Service) WaitAntigravityProbes() {
	if s == nil {
		return
	}
	s.antigravityProbeWg.Wait()
}

func (s *Service) waitAntigravityProbesContext(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.antigravityProbeWg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// asyncProbeAntigravityCapabilities refreshes one credential's Antigravity registration in
// the background, off the registration path. A single upstream fetchAvailableModels
// response is applied in one of two ways:
//
//   - an authoritative catalog (the payload carried a model map) replaces the baseline
//     model list of the credential, so only models upstream still serves are advertised;
//   - otherwise the web search capability hints parsed from the same response are applied
//     to the models that are already registered.
//
// Both outcomes are dropped when the auth was re-registered, disabled or removed while the
// probe was in flight.
func (s *Service) asyncProbeAntigravityCapabilities(ctx context.Context, auth *coreauth.Auth, providerKey string) {
	if auth == nil || auth.ID == "" || auth.Disabled {
		return
	}
	authClone := auth.Clone()
	expectedEpoch := auth.RegistrationEpoch
	expectedPrefix := auth.Prefix
	expectedRegEpoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)

	probeCtx := ctx
	if probeCtx == nil {
		probeCtx = context.Background()
	}

	if s != nil {
		s.antigravityProbeWg.Add(1)
	}

	go func() {
		if s != nil {
			defer s.antigravityProbeWg.Done()
		}
		fetched := s.fetchAntigravityModelsForAuth(probeCtx, authClone)
		hints := fetched.CapabilityHints
		hasCatalog := fetched.AuthoritativeList && len(fetched.Models) > 0
		if !hasCatalog && len(hints.WebSearchModelIDs) == 0 {
			return
		}
		if s == nil {
			return
		}
		if s.coreManager != nil {
			current, exists := s.coreManager.GetByID(authClone.ID)
			if !exists || current == nil || current.Disabled {
				return
			}
			// Version protection: verify auth registration epoch and prefix did not change.
			// Auth.Generation is intentionally NOT checked here because regular request completions
			// increment Generation on every request (via MarkResult), which must not invalidate valid probe results.
			if current.RegistrationEpoch != expectedEpoch || current.Prefix != expectedPrefix {
				return
			}
		}
		if hasCatalog {
			s.replaceAntigravityModelsWithCatalog(authClone, providerKey, expectedRegEpoch, fetched)
			return
		}

		aliasMap := s.buildAntigravityReverseAliasMap(authClone)

		// Atomically update capabilities on existing registered models if epoch matches
		updated := GlobalModelRegistry().ApplyClientModelCapabilities(authClone.ID, expectedRegEpoch, func(modelID string, info *ModelInfo) {
			upstreamID := resolveAntigravityUpstreamModelID(modelID, authClone.Prefix, aliasMap)
			if _, ok := hints.WebSearchModelIDs[upstreamID]; ok {
				info.SupportsWebSearch = true
			}
		})
		if !updated {
			return
		}

		s.reconcileAntigravityProbeResult(authClone.ID)
	}()
}

// replaceAntigravityModelsWithCatalog rebinds one credential to the authoritative model
// catalog reported by the upstream probe, replaying the registration pipeline so the new
// list routes exactly like the baseline registration it supersedes.
//
// The model registry exposes no replace operation carrying an epoch guard, so the
// registration epoch captured when the probe started is re-checked here and RegisterClient
// (which bumps the epoch for any later registration) performs the replacement. An epoch
// mismatch means another registration owns the client now and this stale catalog is
// dropped.
func (s *Service) replaceAntigravityModelsWithCatalog(auth *coreauth.Auth, providerKey string, expectedRegEpoch uint64, fetched antigravityFetchedModels) {
	if s == nil || auth == nil || auth.ID == "" || strings.TrimSpace(providerKey) == "" {
		return
	}
	currentEpoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
	if currentEpoch == 0 || currentEpoch != expectedRegEpoch {
		return
	}
	models := s.buildAntigravityCatalogRegistration(auth, providerKey, fetched)
	if len(models) == 0 {
		return
	}
	s.registerResolvedModelsForAuth(auth, providerKey, models)
	s.reconcileAntigravityProbeResult(auth.ID)
}

// buildAntigravityCatalogRegistration turns a fetched catalog into the final model list for
// one credential by replaying the registration pipeline (excluded models, OAuth aliases,
// plugin models, model prefix) the baseline registration used.
func (s *Service) buildAntigravityCatalogRegistration(auth *coreauth.Auth, providerKey string, fetched antigravityFetchedModels) []*ModelInfo {
	if s == nil || auth == nil {
		return nil
	}
	models := applyAntigravityFetchedModelCapabilities(fetched.Models, fetched.CapabilityHints)
	if len(models) == 0 {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		provider = "antigravity"
	}
	cfg := s.snapshotConfig()
	models = applyExcludedModels(models, antigravityExcludedModels(cfg, auth))
	models = applyOAuthModelAliasForAuth(cfg, provider, auth.AuthKind(), auth.Attributes, models)
	models = s.appendPluginModels(providerKey, models)
	return applyModelPrefixes(models, auth.Prefix, cfg != nil && cfg.ForceModelPrefix)
}

// antigravityExcludedModels resolves the excluded-model patterns that apply to one
// Antigravity credential. The synthesizer pre-merges per-account and global exclusions into
// the "excluded_models" attribute, so that attribute overrides the global configuration.
func antigravityExcludedModels(cfg *config.Config, auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	var excluded []string
	if cfg != nil && !strings.EqualFold(strings.TrimSpace(auth.AuthKind()), "apikey") {
		excluded = cfg.OAuthExcludedModels["antigravity"]
	}
	if auth.Attributes != nil {
		if val, ok := auth.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	return excluded
}

// reconcileAntigravityProbeResult refreshes registry and scheduler state after a probe
// changed the credential's model registration.
func (s *Service) reconcileAntigravityProbeResult(authID string) {
	if s == nil || s.coreManager == nil || authID == "" {
		return
	}
	s.coreManager.ReconcileRegistryModelStates(context.Background(), authID)
	s.coreManager.RefreshSchedulerEntry(authID)
}

// snapshotConfig returns the current service configuration under the config mutex.
// Asynchronous probe work must not read s.cfg directly: configuration reloads replace it
// concurrently.
func (s *Service) snapshotConfig() *config.Config {
	if s == nil {
		return nil
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Service) buildAntigravityReverseAliasMap(auth *coreauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	var cfg *config.Config
	if s != nil {
		s.cfgMu.RLock()
		cfg = s.cfg
		s.cfgMu.RUnlock()
	}
	channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
	aliases := oauthModelAliasesForAuth(cfg, channel, auth.Attributes)
	if len(aliases) == 0 {
		return nil
	}
	aliasMap := make(map[string]string, len(aliases))
	for _, entry := range aliases {
		aliasName := strings.ToLower(strings.TrimSpace(entry.Alias))
		upstreamName := strings.ToLower(strings.TrimSpace(entry.Name))
		if aliasName != "" && upstreamName != "" {
			aliasMap[aliasName] = upstreamName
		}
	}
	return aliasMap
}

func resolveAntigravityUpstreamModelID(modelID string, prefix string, aliasMap map[string]string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	prefix = strings.ToLower(strings.Trim(strings.TrimSpace(prefix), "/"))
	unprefixed := modelID
	if prefix != "" && strings.HasPrefix(modelID, prefix+"/") {
		unprefixed = modelID[len(prefix)+1:]
	}
	if len(aliasMap) > 0 {
		if upstream, ok := aliasMap[unprefixed]; ok && upstream != "" {
			return upstream
		}
	}
	return unprefixed
}
