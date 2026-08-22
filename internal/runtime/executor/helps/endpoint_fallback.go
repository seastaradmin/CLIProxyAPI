package helps

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Endpoint self-healing for OpenAI-compatible providers.
//
// Whether a provider base URL needs a "/v1" path segment is a stable property
// of the upstream: it never flips between requests. When a request hits 404 on
// one variant, the executor retries the alternate variant once within the same
// request; a success is remembered in-process (so concurrent and follow-up
// requests take the working path immediately) and archived to config.yaml (so
// the discovery survives restarts), without requiring a manual protocol probe.

var (
	// endpointArchivePath holds the config file path used for archiving.
	endpointArchivePath atomic.Value // string
	// endpointOverrides maps provider|base|proto to a proven working suffix.
	endpointOverrides sync.Map
	// endpointArchiveInFlight dedupes concurrent archive writes per provider|proto.
	endpointArchiveInFlight sync.Map
)

// SetEndpointArchiveConfigPath wires the config file path used to persist
// endpoint discoveries. Called once at service startup.
func SetEndpointArchiveConfigPath(path string) {
	endpointArchivePath.Store(strings.TrimSpace(path))
}

func endpointOverrideKey(provider, baseURL, proto string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x1f" +
		strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/")) + "\x1f" + proto
}

// ToggleV1EndpointSuffix returns the alternate endpoint suffix with the "/v1"
// path segment added or removed (e.g. "/messages" <-> "/v1/messages").
// Returns "" when there is no meaningful alternate.
func ToggleV1EndpointSuffix(suffix string) string {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return ""
	}
	if strings.HasPrefix(suffix, "/v1/") {
		return strings.TrimPrefix(suffix, "/v1")
	}
	if strings.HasPrefix(suffix, "/") {
		return "/v1" + suffix
	}
	return ""
}

// LookupEndpointOverride returns the runtime-proven endpoint suffix for the
// provider/protocol, or "" when none has been discovered yet.
func LookupEndpointOverride(provider, baseURL, proto string) string {
	if provider == "" || proto == "" {
		return ""
	}
	if v, ok := endpointOverrides.Load(endpointOverrideKey(provider, baseURL, proto)); ok {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return ""
}

// StoreEndpointOverride records a runtime-proven endpoint suffix in memory.
func StoreEndpointOverride(provider, baseURL, proto, suffix string) {
	if provider == "" || proto == "" || suffix == "" {
		return
	}
	endpointOverrides.Store(endpointOverrideKey(provider, baseURL, proto), suffix)
}

// ArchiveProtocolEndpointAsync persists the working endpoint suffix for every
// model of the named openai-compatibility provider (the /v1-ness of a base URL
// is provider-wide). Saving triggers the config watcher hot reload. Concurrent
// calls for the same provider/protocol are deduplicated.
func ArchiveProtocolEndpointAsync(provider, proto, suffix string) {
	path, _ := endpointArchivePath.Load().(string)
	if path == "" || provider == "" || proto == "" || suffix == "" {
		return
	}
	key := strings.ToLower(strings.TrimSpace(provider)) + "\x1f" + proto
	if _, loaded := endpointArchiveInFlight.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer endpointArchiveInFlight.Delete(key)
		cfg, errLoad := config.LoadConfig(path)
		if errLoad != nil {
			log.Warnf("endpoint archive: failed to load config: %v", errLoad)
			return
		}
		modified := false
		for i := range cfg.OpenAICompatibility {
			compat := &cfg.OpenAICompatibility[i]
			if !strings.EqualFold(strings.TrimSpace(compat.Name), strings.TrimSpace(provider)) {
				continue
			}
			update := func(models []config.OpenAICompatibilityModel) {
				for j := range models {
					m := &models[j]
					if m.ProtocolEndpoints[proto] == suffix {
						continue
					}
					if m.ProtocolEndpoints == nil {
						m.ProtocolEndpoints = make(map[string]string)
					}
					m.ProtocolEndpoints[proto] = suffix
					modified = true
				}
			}
			update(compat.Models)
			for k := range compat.APIKeyEntries {
				update(compat.APIKeyEntries[k].Models)
			}
			break
		}
		if !modified {
			return
		}
		if errSave := config.SaveConfigPreserveComments(path, cfg); errSave != nil {
			log.Warnf("endpoint archive: failed to save config: %v", errSave)
			return
		}
		log.Infof("endpoint archive: stored %s endpoint %q for provider %q", proto, suffix, provider)
	}()
}
