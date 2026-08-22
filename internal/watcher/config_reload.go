// config_reload.go implements debounced configuration hot reload.
// It detects material changes and reloads clients when the config changes.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/probe"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func (w *Watcher) stopConfigReloadTimer() {
	w.configReloadMu.Lock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
		w.configReloadTimer = nil
	}
	w.configReloadMu.Unlock()
}

func (w *Watcher) scheduleConfigReload() {
	w.configReloadMu.Lock()
	defer w.configReloadMu.Unlock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
	}
	w.configReloadTimer = time.AfterFunc(configReloadDebounce, func() {
		w.configReloadMu.Lock()
		w.configReloadTimer = nil
		w.configReloadMu.Unlock()
		w.reloadConfigIfChanged()
	})
}

// ReloadConfigIfChanged runs the same config reload path used by filesystem events.
func (w *Watcher) ReloadConfigIfChanged() {
	if w == nil {
		return
	}
	w.reloadConfigIfChanged()
}

func (w *Watcher) reloadConfigIfChanged() {
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		log.Errorf("failed to read config file for hash check: %v", err)
		return
	}
	if len(data) == 0 {
		log.Debugf("ignoring empty config file write event")
		return
	}
	sum := sha256.Sum256(data)
	newHash := hex.EncodeToString(sum[:])

	w.clientsMutex.RLock()
	currentHash := w.lastConfigHash
	w.clientsMutex.RUnlock()

	if currentHash != "" && currentHash == newHash {
		log.Debugf("config file content unchanged (hash match), skipping reload")
		return
	}
	log.Infof("config file changed, reloading: %s", w.configPath)
	if w.reloadConfig() {
		finalHash := newHash
		if updatedData, errRead := os.ReadFile(w.configPath); errRead == nil && len(updatedData) > 0 {
			sumUpdated := sha256.Sum256(updatedData)
			finalHash = hex.EncodeToString(sumUpdated[:])
		} else if errRead != nil {
			log.WithError(errRead).Debug("failed to compute updated config hash after reload")
		}
		w.clientsMutex.Lock()
		w.lastConfigHash = finalHash
		w.clientsMutex.Unlock()
		w.persistConfigAsync()
	}
}

func (w *Watcher) reloadConfig() bool {
	log.Debug("=========================== CONFIG RELOAD ============================")
	log.Debugf("starting config reload from: %s", w.configPath)

	newConfig, errLoadConfig := config.LoadConfig(w.configPath)
	if errLoadConfig != nil {
		log.Errorf("failed to reload config: %v", errLoadConfig)
		return false
	}

	if w.mirroredAuthDir != "" {
		newConfig.AuthDir = w.mirroredAuthDir
	} else {
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(newConfig.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory from config: %v", errResolveAuthDir)
		} else {
			newConfig.AuthDir = resolvedAuthDir
		}
	}

	w.clientsMutex.Lock()
	var oldConfig *config.Config
	_ = yaml.Unmarshal(w.oldConfigYaml, &oldConfig)
	w.oldConfigYaml, _ = yaml.Marshal(newConfig)
	w.config = newConfig
	w.clientsMutex.Unlock()

	var affectedOAuthProviders []string
	if oldConfig != nil {
		_, affectedOAuthProviders = diff.DiffOAuthExcludedModelChanges(oldConfig.OAuthExcludedModels, newConfig.OAuthExcludedModels)
	}

	util.SetLogLevel(newConfig)
	if oldConfig != nil && oldConfig.Debug != newConfig.Debug {
		log.Debugf("log level updated - debug mode changed from %t to %t", oldConfig.Debug, newConfig.Debug)
	}

	if oldConfig != nil {
		details := diff.BuildConfigChangeDetails(oldConfig, newConfig)
		if len(details) > 0 {
			log.Info("config changes detected:")
			for _, d := range details {
				log.Infof("  %s", d)
			}
		} else {
			log.Debugf("no material config field changes detected")
		}
	}

	authDirChanged := oldConfig == nil || oldConfig.AuthDir != newConfig.AuthDir
	retryConfigChanged := oldConfig != nil && (oldConfig.RequestRetry != newConfig.RequestRetry || oldConfig.MaxRetryInterval != newConfig.MaxRetryInterval || oldConfig.MaxRetryCredentials != newConfig.MaxRetryCredentials)
	forceAuthRefresh := oldConfig != nil && (oldConfig.ForceModelPrefix != newConfig.ForceModelPrefix || !reflect.DeepEqual(oldConfig.OAuthModelAlias, newConfig.OAuthModelAlias) || retryConfigChanged)

	log.Infof("config successfully reloaded, triggering client reload")
	w.reloadClients(authDirChanged, affectedOAuthProviders, forceAuthRefresh)

	// Auto-probe openai-compat providers that lack protocol-endpoints.
	w.triggerAutoProbe(oldConfig, newConfig)

	return true
}

// triggerAutoProbe launches background protocol probing for openai-compat providers
// that are new or missing protocol-endpoints configuration.
func (w *Watcher) triggerAutoProbe(oldCfg, newCfg *config.Config) {
	if newCfg == nil || len(newCfg.OpenAICompatibility) == 0 {
		return
	}
	// Collect providers needing probe.
	var targets []probe.ProviderTarget
	for i := range newCfg.OpenAICompatibility {
		compat := &newCfg.OpenAICompatibility[i]
		if compat.Disabled || strings.TrimSpace(compat.BaseURL) == "" {
			continue
		}
		// Skip if all models already have protocol-endpoints.
		if compatHasEndpoints(compat) {
			continue
		}
		// Determine test model.
		model := resolveProbeModel(compat)
		if model == "" {
			continue
		}
		apiKey := ""
		if len(compat.APIKeyEntries) > 0 {
			apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
		}
		targets = append(targets, probe.ProviderTarget{
			Name:    compat.Name,
			BaseURL: compat.BaseURL,
			APIKey:  apiKey,
			Model:   model,
		})
	}
	if len(targets) == 0 {
		return
	}
	configPath := w.configPath
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, target := range targets {
			if ctx.Err() != nil {
				return
			}
			log.Infof("auto-probe: probing provider %q (model=%s)", target.Name, target.Model)
			results := probe.ProbeProvider(ctx, target)
			if len(results) == 0 {
				log.Warnf("auto-probe: provider %q - no protocols succeeded", target.Name)
				continue
			}
			protocols := probe.SupportedProtocols(results)
			endpoints := probe.ProtocolEndpoints(target.BaseURL, results)
			log.Infof("auto-probe: provider %q - protocols=%v endpoints=%v", target.Name, protocols, endpoints)
			// Persist results to config file.
			if errApply := applyProbeResultsToConfig(configPath, target.Name, protocols, endpoints); errApply != nil {
				log.Warnf("auto-probe: failed to persist results for %q: %v", target.Name, errApply)
			}
		}
	}()
}

// compatHasEndpoints returns true if all models in the provider have protocol-endpoints.
func compatHasEndpoints(compat *config.OpenAICompatibility) bool {
	models := compat.Models
	for k := range compat.APIKeyEntries {
		if len(compat.APIKeyEntries[k].Models) > 0 {
			models = compat.APIKeyEntries[k].Models
			break
		}
	}
	if len(models) == 0 {
		return false
	}
	for i := range models {
		if len(models[i].ProtocolEndpoints) == 0 {
			return false
		}
	}
	return true
}

// resolveProbeModel picks the best model to use for probing.
func resolveProbeModel(compat *config.OpenAICompatibility) string {
	// Prefer per-key test-model.
	for k := range compat.APIKeyEntries {
		entry := &compat.APIKeyEntries[k]
		if tm := strings.TrimSpace(entry.TestModel); tm != "" {
			return tm
		}
		if len(entry.Models) > 0 {
			if name := strings.TrimSpace(entry.Models[0].Name); name != "" {
				return name
			}
		}
	}
	// Fall back to provider-level models.
	if len(compat.Models) > 0 {
		if name := strings.TrimSpace(compat.Models[0].Name); name != "" {
			return name
		}
	}
	return ""
}

// applyProbeResultsToConfig writes probe results back to the config file.
func applyProbeResultsToConfig(configPath, providerName string, protocols []string, endpoints map[string]string) error {
	cfg, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		return errLoad
	}
	modified := false
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if !strings.EqualFold(strings.TrimSpace(compat.Name), providerName) {
			continue
		}
		// Update per-key models first, then provider-level.
		updateModels := func(models []config.OpenAICompatibilityModel) {
			for j := range models {
				m := &models[j]
				if len(m.ProtocolEndpoints) > 0 {
					continue // already has endpoints
				}
				if len(protocols) > 0 {
					m.Protocols = protocols
				}
				if len(endpoints) > 0 {
					m.ProtocolEndpoints = endpoints
				}
				modified = true
			}
		}
		for k := range compat.APIKeyEntries {
			if len(compat.APIKeyEntries[k].Models) > 0 {
				updateModels(compat.APIKeyEntries[k].Models)
			}
		}
		if len(compat.Models) > 0 {
			updateModels(compat.Models)
		}
		// Set provider-level protocols if discovered.
		if len(protocols) > 0 && len(compat.Protocols) == 0 {
			compat.Protocols = protocols
			modified = true
		}
		break
	}
	if !modified {
		return nil
	}
	return config.SaveConfigPreserveComments(configPath, cfg)
}
