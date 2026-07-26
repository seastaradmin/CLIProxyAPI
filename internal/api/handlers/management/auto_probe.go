package management

import (
	"context"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	autoProbeDefaultInterval = 60 * time.Minute
	autoProbeMinInterval     = 5 * time.Minute
	// autoProbeStartupDelay schedules the first cycle shortly after boot so
	// credential health is validated without waiting a full interval.
	autoProbeStartupDelay = time.Minute
	autoProbeConcurrency  = 2
)

// StartAutoProbe runs the periodic credential prober until ctx is cancelled.
// Each cycle probes one model per active credential (quarantining on
// credential-level failures via the shared health-check pipeline) and then
// half-opens credentials previously quarantined by the health check so they
// recover automatically once the upstream accepts them again.
func (h *Handler) StartAutoProbe(ctx context.Context) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(autoProbeStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if h.autoProbeEnabled() {
			h.runAutoProbeCycle(ctx)
		}
		timer.Reset(h.autoProbeInterval())
	}
}

func (h *Handler) autoProbeEnabled() bool {
	return h != nil && h.cfg != nil && h.cfg.AutoProbe.Enabled
}

func (h *Handler) autoProbeInterval() time.Duration {
	interval := autoProbeDefaultInterval
	if h != nil && h.cfg != nil && h.cfg.AutoProbe.IntervalMinutes > 0 {
		interval = time.Duration(h.cfg.AutoProbe.IntervalMinutes) * time.Minute
	}
	if interval < autoProbeMinInterval {
		interval = autoProbeMinInterval
	}
	return interval
}

func (h *Handler) runAutoProbeCycle(ctx context.Context) {
	if h.authManager == nil || h.cfg == nil {
		return
	}
	started := time.Now()
	active, quarantined := h.autoProbeTargets()
	log.Infof("auto-probe: cycle start (active=%d quarantined=%d)", len(active), len(quarantined))

	sem := make(chan struct{}, autoProbeConcurrency)
	probe := func(auth *coreauth.Auth) {
		model := h.autoProbeModelForAuth(auth)
		if model == "" {
			return
		}
		status, errMsg := h.runAuthFileHealthCheckOnce(ctx, auth, model, nil)
		if errMsg != "" {
			log.Debugf("auto-probe: %s model=%s status=%s err=%s", auth.ID, model, status, errMsg)
		} else {
			log.Debugf("auto-probe: %s model=%s status=%s", auth.ID, model, status)
		}
	}

	runBatch := func(auths []*coreauth.Auth) {
		done := make(chan struct{}, len(auths))
		for _, auth := range auths {
			auth := auth
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; done <- struct{}{} }()
				if ctx.Err() != nil {
					return
				}
				probe(auth)
			}()
		}
		for range auths {
			<-done
		}
	}

	runBatch(active)

	// Half-open: re-enable health-check quarantined credentials, then probe.
	// A failing probe re-quarantines through the same persistence path.
	recovered := make([]*coreauth.Auth, 0, len(quarantined))
	for _, auth := range quarantined {
		if ctx.Err() != nil {
			return
		}
		auth.Disabled = false
		auth.Status = coreauth.StatusActive
		if auth.Metadata != nil {
			auth.Metadata["disabled"] = false
		}
		if updated, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			log.Warnf("auto-probe: half-open enable failed for %s: %v", auth.ID, errUpdate)
		} else if updated != nil {
			recovered = append(recovered, updated)
		}
	}
	runBatch(recovered)

	log.Infof("auto-probe: cycle done in %s", time.Since(started).Round(time.Millisecond))
}

// autoProbeTargets splits credentials into active ones to watch and
// health-check quarantined ones eligible for half-open recovery.
func (h *Handler) autoProbeTargets() (active, quarantined []*coreauth.Auth) {
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			if auth.LastError != nil && auth.LastError.Code == "management_health_check" {
				quarantined = append(quarantined, auth)
			}
			continue
		}
		active = append(active, auth)
	}
	return active, quarantined
}

// autoProbeModelForAuth picks the routing model to probe for one credential:
// the configured test model when it is actually registered for this
// credential, otherwise the first registered route.
func (h *Handler) autoProbeModelForAuth(auth *coreauth.Auth) string {
	reg := registry.GetGlobalRegistry()
	if reg == nil || auth == nil {
		return ""
	}
	registered := reg.GetModelsForClient(auth.ID)
	if len(registered) == 0 {
		return ""
	}
	preferred := h.autoProbePreferredModel(auth)
	if preferred != "" {
		for _, m := range registered {
			if m != nil && strings.EqualFold(strings.TrimSpace(m.ID), preferred) {
				return preferred
			}
		}
	}
	for _, m := range registered {
		if m == nil {
			continue
		}
		if id := strings.TrimSpace(m.ID); id != "" {
			return id
		}
	}
	return ""
}

func (h *Handler) autoProbePreferredModel(auth *coreauth.Auth) string {
	if h.cfg == nil || auth == nil || auth.Attributes == nil {
		return ""
	}
	compatName := strings.TrimSpace(auth.Attributes["compat_name"])
	apiKey := strings.TrimSpace(auth.Attributes["api_key"])
	if compatName == "" {
		return ""
	}
	for i := range h.cfg.OpenAICompatibility {
		compat := &h.cfg.OpenAICompatibility[i]
		if !strings.EqualFold(strings.TrimSpace(compat.Name), compatName) {
			continue
		}
		if apiKey != "" {
			for j := range compat.APIKeyEntries {
				entry := &compat.APIKeyEntries[j]
				if strings.TrimSpace(entry.APIKey) == apiKey {
					return strings.TrimSpace(entry.TestModel)
				}
			}
		}
		return ""
	}
	return ""
}
