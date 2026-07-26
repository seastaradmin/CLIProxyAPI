package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// configDiagnosticIssue describes one self-contradiction or gap detected in
// the openai-compatibility configuration.
type configDiagnosticIssue struct {
	// Severity is one of "error", "warning", "info".
	Severity string `json:"severity"`
	// Code is a machine-readable issue identifier.
	Code string `json:"code"`
	// Provider is the openai-compat entry name.
	Provider string `json:"provider"`
	// KeyIndex is the API key entry index when the issue is key-scoped.
	KeyIndex *int `json:"key_index,omitempty"`
	// Model is the affected model routing or upstream name when applicable.
	Model string `json:"model,omitempty"`
	// Message is a human-readable description.
	Message string `json:"message"`
}

// ConfigDiagnostics scans the live config for self-contradictory or incomplete
// openai-compatibility settings that historically caused silent failures.
func (h *Handler) ConfigDiagnostics(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "handler unavailable"})
		return
	}
	issues := diagnoseOpenAICompat(h.cfg)
	c.JSON(http.StatusOK, gin.H{"issues": issues, "count": len(issues)})
}

// routeNamesForModel returns the client-facing routing names produced by one
// model entry: alias replaces the name unless fork keeps both.
func routeNamesForModel(m *config.OpenAICompatibilityModel) []string {
	alias := strings.TrimSpace(m.Alias)
	name := strings.TrimSpace(m.Name)
	if alias == "" {
		if name == "" {
			return nil
		}
		return []string{name}
	}
	if m.Fork && name != "" && !strings.EqualFold(alias, name) {
		return []string{alias, name}
	}
	return []string{alias}
}

// diagMatchPattern reports whether an exclusion pattern (with optional '*'
// wildcards) matches the given model name, case-insensitively.
func diagMatchPattern(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" || value == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	rest := value
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(rest, part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false
		}
		rest = rest[idx+len(part):]
	}
	if !strings.HasSuffix(pattern, "*") && parts[len(parts)-1] != "" {
		return strings.HasSuffix(value, parts[len(parts)-1])
	}
	return true
}

func diagnoseOpenAICompat(cfg *config.Config) []configDiagnosticIssue {
	issues := make([]configDiagnosticIssue, 0)
	if cfg == nil {
		return issues
	}
	add := func(severity, code, provider string, keyIndex *int, model, message string) {
		issues = append(issues, configDiagnosticIssue{
			Severity: severity,
			Code:     code,
			Provider: provider,
			KeyIndex: keyIndex,
			Model:    model,
			Message:  message,
		})
	}

	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		provider := strings.TrimSpace(compat.Name)
		if provider == "" {
			provider = fmt.Sprintf("#%d", i+1)
		}
		if compat.Disabled {
			continue
		}
		if len(compat.APIKeyEntries) == 0 {
			add("warning", "no_api_keys", provider, nil, "",
				"供应商没有任何 API 密钥条目，不会参与路由")
		}

		checkModelList := func(models []config.OpenAICompatibilityModel, keyIndex *int, excluded []string) {
			seenRoutes := make(map[string]int)
			for j := range models {
				m := &models[j]
				name := strings.TrimSpace(m.Name)
				alias := strings.TrimSpace(m.Alias)
				label := name
				if label == "" {
					label = alias
				}
				if label == "" {
					continue
				}

				// fork without alias is a no-op.
				if m.Fork && alias == "" {
					add("warning", "fork_without_alias", provider, keyIndex, label,
						"勾选了 fork 但没有配置别名，fork 不产生任何效果")
				}

				routes := routeNamesForModel(m)
				for _, route := range routes {
					key := strings.ToLower(route)
					seenRoutes[key]++
					if seenRoutes[key] == 2 {
						add("warning", "duplicate_route", provider, keyIndex, route,
							"同一列表中路由名重复，后者不会生效")
					}
					// Route excluded by the same key's exclusion list makes
					// the credential silently dead for this model.
					for _, pattern := range excluded {
						pattern = strings.TrimSpace(pattern)
						if pattern == "" || pattern == "*" {
							continue
						}
						if diagMatchPattern(pattern, route) {
							add("error", "route_excluded", provider, keyIndex, route,
								fmt.Sprintf("路由名 %q 被本密钥的排除规则 %q 命中，该凭证在此模型上不可用", route, pattern))
						}
					}
				}

				// Protocol declarations without a discovered endpoint for
				// non-chat protocols mean the executor falls back to guessing.
				if len(m.Protocols) > 0 {
					normalized := util.NormalizeUpstreamProtocols(m.Protocols)
					for _, proto := range normalized {
						if proto == "chat" {
							continue
						}
						if m.ProtocolEndpoints[proto] == "" {
							add("info", "missing_endpoint", provider, keyIndex, label,
								fmt.Sprintf("协议 %s 已标注但缺少探测存档路径，建议运行一次「一键探测」", proto))
						}
					}
				} else {
					add("info", "never_probed", provider, keyIndex, label,
						"模型没有任何协议标注，建议运行一次「一键探测」确认支持范围")
				}
			}
		}

		checkModelList(compat.Models, nil, nil)
		for k := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[k]
			keyIndex := k
			models := entry.Models
			if len(models) == 0 {
				// Key inherits provider models; still check exclusions
				// against the provider-level list.
				models = compat.Models
			}
			checkModelList(models, &keyIndex, entry.ExcludedModels)
		}
	}
	return issues
}
