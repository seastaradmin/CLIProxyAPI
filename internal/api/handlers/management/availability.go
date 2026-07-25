package management

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func init() {
	coreusage.EnsureAvailabilityPluginRegistered()
}

type availabilityGroup struct {
	Provider     string  `json:"provider"`
	Supplier     string  `json:"supplier,omitempty"`
	Models       int     `json:"models"`
	Auths        int     `json:"auths,omitempty"`
	Total        int64   `json:"total"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	RateLimited  int64   `json:"rate_limited"`
	Availability float64 `json:"availability"`
	LatencyP50MS float64 `json:"latency_p50_ms"`
	TTFTP50MS    float64 `json:"ttft_p50_ms"`
	TPS          float64 `json:"tps"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	Status       string  `json:"status"`
	HasTraffic   bool    `json:"has_traffic"`
	IdleModels   int     `json:"idle_models"`
}

func aggregateAvailabilityGroups(items []coreusage.AvailabilitySnapshot) []availabilityGroup {
	groupsMap := map[string]*availabilityGroup{}
	latencySum := map[string]float64{}
	ttftSum := map[string]float64{}
	tpsSum := map[string]float64{}
	cacheSum := map[string]float64{}
	metricN := map[string]int{}

	for _, snap := range items {
		p := snap.Key.Provider
		if p == "" {
			p = "unknown"
		}
		g, ok := groupsMap[p]
		if !ok {
			g = &availabilityGroup{Provider: p, Status: "no_data"}
			groupsMap[p] = g
		}
		g.Total += snap.Total
		g.Success += snap.Success
		g.Failed += snap.Failed
		g.RateLimited += snap.RateLimited
		if snap.HasTraffic {
			g.Models++
			g.HasTraffic = true
		} else {
			g.IdleModels++
		}
		if snap.LatencyP50MS > 0 || snap.TTFTP50MS > 0 || snap.TPS > 0 {
			latencySum[p] += snap.LatencyP50MS
			ttftSum[p] += snap.TTFTP50MS
			tpsSum[p] += snap.TPS
			cacheSum[p] += snap.CacheHitRate
			metricN[p]++
		}
	}
	groups := make([]availabilityGroup, 0, len(groupsMap))
	for p, g := range groupsMap {
		if g.Total > 0 {
			g.Availability = float64(g.Success) / float64(g.Total)
			g.Status = "running"
			if g.Availability < 0.95 || g.RateLimited > 0 {
				g.Status = "degraded"
			}
		} else {
			g.Status = "no_data"
			g.Availability = 0
		}
		if n := metricN[p]; n > 0 {
			g.LatencyP50MS = latencySum[p] / float64(n)
			g.TTFTP50MS = ttftSum[p] / float64(n)
			g.TPS = tpsSum[p] / float64(n)
			g.CacheHitRate = cacheSum[p] / float64(n)
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].HasTraffic != groups[j].HasTraffic {
			return groups[i].HasTraffic
		}
		if groups[i].Total != groups[j].Total {
			return groups[i].Total > groups[j].Total
		}
		return groups[i].Provider < groups[j].Provider
	})
	return groups
}

// aggregateAvailabilitySupplierGroups groups snapshots only by explicit operator supplier tags.
// Untagged credentials are omitted so the "按来源标签" wall stays empty until tags exist.
func aggregateAvailabilitySupplierGroups(items []coreusage.AvailabilitySnapshot) []availabilityGroup {
	groupsMap := map[string]*availabilityGroup{}
	authSeen := map[string]map[string]struct{}{}
	latencySum := map[string]float64{}
	ttftSum := map[string]float64{}
	tpsSum := map[string]float64{}
	cacheSum := map[string]float64{}
	metricN := map[string]int{}

	for _, snap := range items {
		s := strings.TrimSpace(snap.Key.Supplier)
		if s == "" {
			continue
		}
		g, ok := groupsMap[s]
		if !ok {
			g = &availabilityGroup{Supplier: s, Provider: strings.TrimSpace(snap.Key.Provider), Status: "no_data"}
			groupsMap[s] = g
			authSeen[s] = map[string]struct{}{}
		}
		g.Total += snap.Total
		g.Success += snap.Success
		g.Failed += snap.Failed
		g.RateLimited += snap.RateLimited
		if snap.HasTraffic {
			g.Models++
			g.HasTraffic = true
		} else {
			g.IdleModels++
		}
		authKey := strings.TrimSpace(snap.Key.AuthIndex)
		if authKey == "" {
			authKey = strings.TrimSpace(snap.Key.AuthID)
		}
		if authKey != "" {
			authSeen[s][authKey] = struct{}{}
		}
		if snap.LatencyP50MS > 0 || snap.TTFTP50MS > 0 || snap.TPS > 0 {
			latencySum[s] += snap.LatencyP50MS
			ttftSum[s] += snap.TTFTP50MS
			tpsSum[s] += snap.TPS
			cacheSum[s] += snap.CacheHitRate
			metricN[s]++
		}
	}
	groups := make([]availabilityGroup, 0, len(groupsMap))
	for s, g := range groupsMap {
		g.Auths = len(authSeen[s])
		if g.Total > 0 {
			g.Availability = float64(g.Success) / float64(g.Total)
			g.Status = "running"
			if g.Availability < 0.95 || g.RateLimited > 0 {
				g.Status = "degraded"
			}
		} else {
			g.Status = "no_data"
			g.Availability = 0
		}
		if n := metricN[s]; n > 0 {
			g.LatencyP50MS = latencySum[s] / float64(n)
			g.TTFTP50MS = ttftSum[s] / float64(n)
			g.TPS = tpsSum[s] / float64(n)
			g.CacheHitRate = cacheSum[s] / float64(n)
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].HasTraffic != groups[j].HasTraffic {
			return groups[i].HasTraffic
		}
		if groups[i].Total != groups[j].Total {
			return groups[i].Total > groups[j].Total
		}
		return groups[i].Supplier < groups[j].Supplier
	})
	return groups
}

// GetAvailability returns sliding-window availability metrics grouped by auth/channel × model.
// It always includes registered channels/models even when they have no traffic yet.
// Query:
//   - window: duration string (default 1h), e.g. 15m, 1h, 24h
//   - buckets: sparkline bucket count (default 24)
//   - provider / auth_index / model: optional filters
//   - include_idle: default true; set false to only return keys with traffic
func (h *Handler) GetAvailability(c *gin.Context) {
	window := parseAvailabilityWindow(c.Query("window"))
	bucketCount := parseAvailabilityBuckets(c.Query("buckets"))
	providerFilter := strings.TrimSpace(c.Query("provider"))
	supplierFilter := strings.TrimSpace(c.Query("supplier"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	modelFilter := strings.TrimSpace(c.Query("model"))
	includeIdle := !strings.EqualFold(strings.TrimSpace(c.Query("include_idle")), "false")

	now := time.Now()
	agg := coreusage.DefaultAvailabilityAggregator()
	coreusage.EnsureAvailabilityPluginRegistered()
	snapshots := agg.Snapshot(now, window, bucketCount)

	// Index live traffic snapshots by stable key id.
	byID := make(map[string]coreusage.AvailabilitySnapshot, len(snapshots))
	for _, snap := range snapshots {
		snap.Key.Provider = normalizeAvailabilityProvider(snap.Key.Provider)
		byID[availabilityKeyID(snap.Key)] = snap
	}

	// Seed missing registered channel×model pairs as no_data, and attach protocols
	// metadata onto existing traffic snapshots when available.
	for _, key := range h.registeredAvailabilityKeys() {
		id := availabilityKeyID(key)
		if snap, ok := byID[id]; ok {
			changed := false
			if len(snap.Key.Protocols) == 0 && len(key.Protocols) > 0 {
				snap.Key.Protocols = append([]string(nil), key.Protocols...)
				changed = true
			}
			if strings.TrimSpace(snap.Key.Supplier) == "" && strings.TrimSpace(key.Supplier) != "" {
				snap.Key.Supplier = strings.TrimSpace(key.Supplier)
				changed = true
			}
			if changed {
				byID[id] = snap
			}
			continue
		}
		if includeIdle {
			byID[id] = coreusage.EmptySnapshot(key, now, window, bucketCount)
		}
	}

	items := make([]coreusage.AvailabilitySnapshot, 0, len(byID))
	var totalReq, totalOK int64
	for _, snap := range byID {
		if providerFilter != "" && !strings.EqualFold(snap.Key.Provider, providerFilter) {
			continue
		}
		if supplierFilter != "" {
			sup := strings.TrimSpace(snap.Key.Supplier)
			if sup == "" {
				sup = strings.TrimSpace(snap.Key.Provider)
				if strings.HasPrefix(strings.ToLower(sup), "openai-compatible-") {
					sup = sup[len("openai-compatible-"):]
				}
			}
			if !strings.EqualFold(sup, supplierFilter) {
				continue
			}
		}
		if authIndexFilter != "" && snap.Key.AuthIndex != authIndexFilter && snap.Key.AuthID != authIndexFilter {
			continue
		}
		if modelFilter != "" && snap.Key.Model != modelFilter && snap.Key.Alias != modelFilter {
			continue
		}
		items = append(items, snap)
		totalReq += snap.Total
		totalOK += snap.Success
	}

	sort.Slice(items, func(i, j int) bool {
		// Traffic first, then provider/model.
		if items[i].HasTraffic != items[j].HasTraffic {
			return items[i].HasTraffic
		}
		if items[i].Total != items[j].Total {
			return items[i].Total > items[j].Total
		}
		if items[i].Key.Provider != items[j].Key.Provider {
			return items[i].Key.Provider < items[j].Key.Provider
		}
		if items[i].Key.AuthIndex != items[j].Key.AuthIndex {
			return items[i].Key.AuthIndex < items[j].Key.AuthIndex
		}
		return items[i].Key.Model < items[j].Key.Model
	})

	overall := 0.0
	if totalReq > 0 {
		overall = float64(totalOK) / float64(totalReq)
	}

	// Group by provider (channel family) and supplier label for card-wall summary.
	groups := aggregateAvailabilityGroups(items)
	supplierGroups := aggregateAvailabilitySupplierGroups(items)

	c.JSON(http.StatusOK, gin.H{
		"window":               window.String(),
		"buckets":              bucketCount,
		"generated_at":         now,
		"overall_availability": overall,
		"total_requests":       totalReq,
		"total_success":        totalOK,
		"include_idle":         includeIdle,
		"registered_models":    len(items),
		"groups":               groups,
		"supplier_groups":      supplierGroups,
		"items":                items,
	})
}

// normalizeAvailabilityProvider collapses display-only openai-compat names onto the
// canonical "openai-compatible-<name>" key used by runtime registration.
func normalizeAvailabilityProvider(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return provider
	}
	lower := strings.ToLower(provider)
	// Already canonical / native providers: leave alone.
	if strings.HasPrefix(lower, "openai-compatible-") || lower == "openai-compatibility" {
		return lower
	}
	switch lower {
	case "antigravity", "claude", "anthropic", "gemini", "aistudio", "codex", "openai", "kimi", "xai", "vertex", "iflow", "qwen":
		return lower
	}
	// Bare openai-compat channel labels (e.g. "薄荷") → openai-compatible-薄荷
	// Keep original case for CJK names after the prefix for readability.
	return "openai-compatible-" + provider
}

func availabilityKeyID(key coreusage.AvailabilityKey) string {
	provider := normalizeAvailabilityProvider(key.Provider)
	channel := strings.TrimSpace(key.AuthIndex)
	if channel == "" {
		channel = strings.TrimSpace(key.AuthID)
	}
	if channel == "" {
		channel = provider
	}
	model := strings.TrimSpace(key.Model)
	if model == "" {
		model = "unknown"
	}
	return strings.ToLower(provider) + "\x1f" + channel + "\x1f" + model
}

// registeredAvailabilityKeys walks auth manager + model registry and returns every
// currently registered channel/auth × model pair, including idle ones.
func (h *Handler) registeredAvailabilityKeys() []coreusage.AvailabilityKey {
	if h == nil {
		return nil
	}
	reg := registry.GetGlobalRegistry()
	seen := make(map[string]coreusage.AvailabilityKey)

	add := func(provider, authID, authIndex, model, supplier string, protocols []string) {
		provider = normalizeAvailabilityProvider(provider)
		model = strings.TrimSpace(model)
		if provider == "" || model == "" {
			return
		}
		key := coreusage.AvailabilityKey{
			Provider:  provider,
			AuthID:    strings.TrimSpace(authID),
			AuthIndex: strings.TrimSpace(authIndex),
			Supplier:  strings.TrimSpace(supplier),
			Model:     model,
			Protocols: append([]string(nil), protocols...),
		}
		id := availabilityKeyID(key)
		if existing, ok := seen[id]; ok {
			// Prefer entry that has protocols / auth index / supplier.
			if len(existing.Protocols) == 0 && len(key.Protocols) > 0 {
				existing.Protocols = key.Protocols
			}
			if existing.AuthIndex == "" && key.AuthIndex != "" {
				existing.AuthIndex = key.AuthIndex
				existing.AuthID = key.AuthID
			}
			if existing.Supplier == "" && key.Supplier != "" {
				existing.Supplier = key.Supplier
			}
			seen[id] = existing
			return
		}
		seen[id] = key
	}

	// Resolve openai-compat protocols for a channel/model from live config.
	// baseURL disambiguates same-name providers (e.g. mimo OpenAI vs Anthropic).
	// authAPIKey scopes per-key model lists to the credential being listed.
	resolveCompatProtocols := func(compatName, providerKey, baseURL, authAPIKey, model string) []string {
		if h.cfg == nil {
			return nil
		}
		model = strings.TrimSpace(model)
		baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		authAPIKey = strings.TrimSpace(authAPIKey)
		matchModelProtocols := func(list []config.OpenAICompatibilityModel) []string {
			for j := range list {
				m := &list[j]
				actual := strings.TrimSpace(m.Name)
				alias := strings.TrimSpace(m.Alias)
				if (actual != "" && strings.EqualFold(actual, model)) ||
					(alias != "" && strings.EqualFold(alias, model)) {
					if len(m.Protocols) > 0 {
						return util.NormalizeUpstreamProtocols(m.Protocols)
					}
				}
			}
			return nil
		}
		// Collect candidates by name/provider first; prefer baseURL match when present.
		type cand struct {
			protocols []string
			base      string
		}
		var candidates []cand
		for i := range h.cfg.OpenAICompatibility {
			compat := &h.cfg.OpenAICompatibility[i]
			if compat.Disabled {
				continue
			}
			name := strings.TrimSpace(compat.Name)
			if name == "" {
				continue
			}
			match := strings.EqualFold(name, compatName)
			if !match && providerKey != "" {
				match = strings.EqualFold(util.OpenAICompatibleProviderKey(strings.ToLower(name)), providerKey) ||
					strings.EqualFold(normalizeAvailabilityProvider(name), providerKey)
			}
			if !match {
				continue
			}
			// Model-level override: prefer the key entry owning this credential,
			// then the provider-level list, then any other key entry list.
			modelProtocols := []string(nil)
			if authAPIKey != "" {
				for k := range compat.APIKeyEntries {
					if strings.TrimSpace(compat.APIKeyEntries[k].APIKey) == authAPIKey {
						modelProtocols = matchModelProtocols(compat.APIKeyEntries[k].Models)
						break
					}
				}
			}
			if modelProtocols == nil {
				modelProtocols = matchModelProtocols(compat.Models)
			}
			if modelProtocols == nil {
				for k := range compat.APIKeyEntries {
					if p := matchModelProtocols(compat.APIKeyEntries[k].Models); p != nil {
						modelProtocols = p
						break
					}
				}
			}
			protos := modelProtocols
			if protos == nil && len(compat.Protocols) > 0 {
				protos = util.NormalizeUpstreamProtocols(compat.Protocols)
			}
			candidates = append(candidates, cand{
				protocols: protos,
				base:      strings.TrimRight(strings.TrimSpace(compat.BaseURL), "/"),
			})
		}
		if len(candidates) == 0 {
			return nil
		}
		if baseURL != "" {
			for _, c := range candidates {
				if c.base != "" && strings.EqualFold(c.base, baseURL) {
					return c.protocols
				}
				// Also match when one is a prefix of the other (…/anthropic vs …/anthropic/v1).
				if c.base != "" && (strings.HasPrefix(strings.ToLower(baseURL), strings.ToLower(c.base)) ||
					strings.HasPrefix(strings.ToLower(c.base), strings.ToLower(baseURL))) {
					return c.protocols
				}
			}
		}
		// Fallback: if only one name match, use it; if multiple without base match, omit ambiguous.
		if len(candidates) == 1 {
			return candidates[0].protocols
		}
		return nil
	}

	// 1) Per-auth models from auth manager + registry client mapping.
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				continue
			}
			auth.EnsureIndex()
			provider := strings.TrimSpace(auth.Provider)
			if provider == "" {
				continue
			}
			compatName := ""
			providerKey := normalizeAvailabilityProvider(provider)
			baseURL := ""
			apiKey := ""
			supplier := ""
			if auth.Attributes != nil {
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
				baseURL = strings.TrimSpace(auth.Attributes["base_url"])
				apiKey = strings.TrimSpace(auth.Attributes["api_key"])
				supplier = strings.TrimSpace(auth.Attributes["supplier"])
				if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
					providerKey = normalizeAvailabilityProvider(v)
				}
			}
			if supplier == "" && auth.Metadata != nil {
				if raw, ok := auth.Metadata["supplier"].(string); ok {
					supplier = strings.TrimSpace(raw)
				}
			}
			if compatName == "" {
				compatName = strings.TrimSpace(auth.Label)
			}

			modelIDs := map[string]struct{}{}
			// Prefer models declared on the matching openai-compat config entry (by name+baseURL).
			// This keeps Anthropic-only and OpenAI-only channels separate even when names collide.
			if models := compatConfiguredModels(h.cfg, compatName, providerKey, baseURL, apiKey); len(models) > 0 {
				for _, modelID := range models {
					modelIDs[modelID] = struct{}{}
				}
			}
			// Registry models for this auth (may include aliases / remote registrations).
			for _, m := range reg.GetModelsForClient(auth.ID) {
				if m == nil || strings.TrimSpace(m.ID) == "" {
					continue
				}
				modelIDs[strings.TrimSpace(m.ID)] = struct{}{}
			}
			// OAuth / native provider catalogs when this auth has no explicit model list yet.
			if len(modelIDs) == 0 {
				for _, m := range reg.GetAvailableModelsByProvider(strings.ToLower(provider)) {
					if m == nil || strings.TrimSpace(m.ID) == "" {
						continue
					}
					modelIDs[strings.TrimSpace(m.ID)] = struct{}{}
				}
			}
			if len(modelIDs) == 0 {
				for _, modelID := range staticModelsForProvider(provider) {
					modelIDs[modelID] = struct{}{}
				}
			}
			// Channel-level protocols from auth attribute (synthesized from config).
			// Omit pure default [chat] so the UI only shows customized protocol sets.
			channelProtocols := []string(nil)
			if auth.Attributes != nil {
				if raw := strings.TrimSpace(auth.Attributes["protocols"]); raw != "" {
					parsed := util.ParseProtocolsCSV(raw)
					if len(parsed) > 1 || (len(parsed) == 1 && parsed[0] != "chat") {
						channelProtocols = parsed
					}
				}
			}
			if len(modelIDs) == 0 {
				// Last resort: keep the channel visible.
				protos := resolveCompatProtocols(compatName, providerKey, baseURL, apiKey, "unknown")
				if protos == nil {
					protos = channelProtocols
				}
				add(provider, auth.ID, auth.Index, "unknown", supplier, protos)
				continue
			}
			for modelID := range modelIDs {
				protos := resolveCompatProtocols(compatName, providerKey, baseURL, apiKey, modelID)
				if protos == nil {
					protos = channelProtocols
				}
				// If still nil but config has any non-default channel protocols via resolve with empty model path already tried.
				add(provider, auth.ID, auth.Index, modelID, supplier, protos)
			}
		}
	}

	// 2) Provider-level models from the openai handler catalog.
	// IMPORTANT: do NOT also seed by owned_by — for openai-compat channels owned_by is the
	// human name (e.g. "薄荷") while registry providers already use the canonical key
	// ("openai-compatible-薄荷"). Seeding both creates duplicate cards.
	for _, raw := range reg.GetAvailableModels("openai") {
		if raw == nil {
			continue
		}
		modelID, _ := raw["id"].(string)
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		providers := reg.GetModelProviders(modelID)
		if len(providers) == 0 {
			// Fallback only when registry has no provider mapping yet.
			if ownedBy, _ := raw["owned_by"].(string); strings.TrimSpace(ownedBy) != "" {
				own := strings.TrimSpace(ownedBy)
				protos := resolveCompatProtocols(own, normalizeAvailabilityProvider(own), "", "", modelID)
				add(own, "", "", modelID, "", protos)
			} else {
				add("unknown", "", "", modelID, "", nil)
			}
			continue
		}
		for _, provider := range providers {
			pn := normalizeAvailabilityProvider(provider)
			// Skip anonymous openai-compat rows: those channels are seeded per-auth above
			// (with auth_index + correct baseURL protocols). Catalog-only rows are confusing.
			if strings.HasPrefix(strings.ToLower(pn), "openai-compatible-") || strings.EqualFold(pn, "openai-compatibility") {
				continue
			}
			add(provider, "", "", modelID, "", nil)
		}
	}

	out := make([]coreusage.AvailabilityKey, 0, len(seen))
	for _, key := range seen {
		out = append(out, key)
	}
	return out
}

// compatConfiguredModels returns model IDs declared on the openai-compat config entry
// matching name/provider and optional baseURL.
func compatConfiguredModels(cfg *config.Config, compatName, providerKey, baseURL, apiKey string) []string {
	if cfg == nil {
		return nil
	}
	compatName = strings.TrimSpace(compatName)
	providerKey = normalizeAvailabilityProvider(providerKey)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	var out []string
	var ambiguous [][]string
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		name := strings.TrimSpace(compat.Name)
		if name == "" {
			continue
		}
		match := compatName != "" && strings.EqualFold(name, compatName)
		if !match && providerKey != "" {
			match = strings.EqualFold(util.OpenAICompatibleProviderKey(strings.ToLower(name)), providerKey) ||
				strings.EqualFold(normalizeAvailabilityProvider(name), providerKey)
		}
		if !match {
			continue
		}
		configuredModels := compat.Models
		if apiKey != "" {
			for j := range compat.APIKeyEntries {
				entry := &compat.APIKeyEntries[j]
				if strings.TrimSpace(entry.APIKey) != apiKey {
					continue
				}
				if len(entry.Models) > 0 {
					configuredModels = entry.Models
				}
				break
			}
		}
		models := make([]string, 0, len(configuredModels))
		seenIDs := make(map[string]struct{}, len(configuredModels))
		appendID := func(id string) {
			id = strings.TrimSpace(id)
			if id == "" {
				return
			}
			key := strings.ToLower(id)
			if _, ok := seenIDs[key]; ok {
				return
			}
			seenIDs[key] = struct{}{}
			models = append(models, id)
		}
		for j := range configuredModels {
			m := &configuredModels[j]
			alias := strings.TrimSpace(m.Alias)
			name := strings.TrimSpace(m.Name)
			// Rows are keyed by routing name: the alias replaces the upstream
			// name unless fork keeps both routable.
			if alias != "" {
				appendID(alias)
				if m.Fork {
					appendID(name)
				}
				continue
			}
			appendID(name)
		}
		cbase := strings.TrimRight(strings.TrimSpace(compat.BaseURL), "/")
		if baseURL != "" && cbase != "" {
			if strings.EqualFold(cbase, baseURL) ||
				strings.HasPrefix(strings.ToLower(baseURL), strings.ToLower(cbase)) ||
				strings.HasPrefix(strings.ToLower(cbase), strings.ToLower(baseURL)) {
				return models
			}
			// name matched but base did not — keep as ambiguous fallback only if nothing else matches.
			ambiguous = append(ambiguous, models)
			continue
		}
		// No baseURL to disambiguate.
		ambiguous = append(ambiguous, models)
	}
	if len(ambiguous) == 1 {
		return ambiguous[0]
	}
	// Multiple same-name channels without base match: do not merge (would mix OpenAI/Anthropic models).
	_ = out
	return nil
}

func staticModelsForProvider(provider string) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var models []*registry.ModelInfo
	switch provider {
	case "antigravity":
		models = registry.GetAntigravityModels()
	case "claude", "anthropic":
		models = registry.GetClaudeModels()
	case "gemini":
		models = registry.GetGeminiModels()
	case "aistudio":
		models = registry.GetAIStudioModels()
	case "codex", "openai":
		models = registry.GetCodexProModels()
	case "kimi":
		models = registry.GetKimiModels()
	case "xai":
		models = registry.GetXAIModels()
	case "vertex":
		models = registry.GetGeminiVertexModels()
	default:
		return nil
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m == nil || strings.TrimSpace(m.ID) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(m.ID))
	}
	return out
}

func parseAvailabilityWindow(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return coreusage.DefaultAvailabilityWindow
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return coreusage.DefaultAvailabilityWindow
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func parseAvailabilityBuckets(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 24
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 24
	}
	if n > 96 {
		return 96
	}
	return n
}
