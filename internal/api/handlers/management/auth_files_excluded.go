package management

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func authFileExcludedModels(metadata map[string]any) []string {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["excluded_models"]
	if !ok {
		raw = metadata["excluded-models"]
	}
	var values []string
	switch typed := raw.(type) {
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			if value, okString := item.(string); okString {
				values = append(values, value)
			}
		}
	case string:
		values = strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == '\n' })
	}
	return config.NormalizeExcludedModels(values)
}

func syncAuthFileExcludedModels(auth *coreauth.Auth, cfg *config.Config) {
	if auth == nil || cfg == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	delete(auth.Attributes, "excluded_models")
	delete(auth.Attributes, "excluded_models_hash")
	synthesizer.ApplyAuthExcludedModelsMeta(
		auth,
		cfg,
		authFileExcludedModels(auth.Metadata),
		auth.AuthKind(),
	)
}
