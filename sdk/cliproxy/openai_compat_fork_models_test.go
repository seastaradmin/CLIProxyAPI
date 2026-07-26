package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func modelIDs(models []*ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func TestBuildOpenAICompatConfigModels_AliasReplacesName(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "acme",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-a", Alias: "route-a"},
		},
	}
	got := modelIDs(buildOpenAICompatibilityConfigModels(compat))
	if len(got) != 1 || got[0] != "route-a" {
		t.Fatalf("route IDs = %v, want [route-a] (alias replaces name)", got)
	}
}

func TestBuildOpenAICompatConfigModels_ForkKeepsBothRoutes(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "acme",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-a", Alias: "route-a", Fork: true},
		},
	}
	models := buildOpenAICompatibilityConfigModels(compat)
	got := modelIDs(models)
	if len(got) != 2 || got[0] != "route-a" || got[1] != "upstream-a" {
		t.Fatalf("route IDs = %v, want [route-a upstream-a]", got)
	}
	for _, m := range models {
		if m.Name != "upstream-a" {
			t.Fatalf("route %s upstream = %q, want upstream-a", m.ID, m.Name)
		}
	}
}

func TestBuildOpenAICompatConfigModels_ForkIgnoredWithoutAliasOrSameName(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "acme",
		Models: []config.OpenAICompatibilityModel{
			{Name: "plain", Fork: true},
			{Name: "same", Alias: "SAME", Fork: true},
		},
	}
	got := modelIDs(buildOpenAICompatibilityConfigModels(compat))
	if len(got) != 2 || got[0] != "plain" || got[1] != "SAME" {
		t.Fatalf("route IDs = %v, want [plain SAME] (fork no-op cases)", got)
	}
}

func TestBuildOpenAICompatConfigModels_DedupesRouteIDs(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "acme",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-a", Alias: "route-x", Fork: true},
			// Duplicate-row style fork emulation must not double-register.
			{Name: "upstream-a", Alias: ""},
			{Name: "other", Alias: "route-x"},
		},
	}
	got := modelIDs(buildOpenAICompatibilityConfigModels(compat))
	if len(got) != 2 || got[0] != "route-x" || got[1] != "upstream-a" {
		t.Fatalf("route IDs = %v, want [route-x upstream-a] (deduped)", got)
	}
}
