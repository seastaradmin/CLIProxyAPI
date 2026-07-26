package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func diagFixtureConfig() *config.Config {
	return &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "acme",
			BaseURL: "https://acme.example/v1",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
				APIKey: "key-a",
				Models: []config.OpenAICompatibilityModel{
					// Route (alias) excluded by the same key: dead credential.
					{Name: "upstream-a", Alias: "route-a"},
					// fork without alias: no-op.
					{Name: "plain", Fork: true, Protocols: []string{"chat"}},
					// responses declared without endpoint archive.
					{Name: "resp-model", Protocols: []string{"responses"}},
					// never probed.
					{Name: "fresh-model"},
					// duplicate route.
					{Name: "dup"},
					{Name: "dup2", Alias: "dup"},
				},
				ExcludedModels: []string{"route-a"},
			}},
		}},
	}
}

func issueCodes(issues []configDiagnosticIssue) map[string]int {
	out := make(map[string]int)
	for _, issue := range issues {
		out[issue.Code]++
	}
	return out
}

func TestDiagnoseOpenAICompat_DetectsIssueFamilies(t *testing.T) {
	issues := diagnoseOpenAICompat(diagFixtureConfig())
	codes := issueCodes(issues)

	if codes["route_excluded"] == 0 {
		t.Fatalf("expected route_excluded issue, got %v", codes)
	}
	if codes["fork_without_alias"] == 0 {
		t.Fatalf("expected fork_without_alias issue, got %v", codes)
	}
	if codes["missing_endpoint"] == 0 {
		t.Fatalf("expected missing_endpoint issue, got %v", codes)
	}
	if codes["never_probed"] == 0 {
		t.Fatalf("expected never_probed issue, got %v", codes)
	}
	if codes["duplicate_route"] == 0 {
		t.Fatalf("expected duplicate_route issue, got %v", codes)
	}
}

func TestDiagnoseOpenAICompat_SkipsDisabledAndWildcard(t *testing.T) {
	cfg := diagFixtureConfig()
	cfg.OpenAICompatibility[0].Disabled = true
	if issues := diagnoseOpenAICompat(cfg); len(issues) != 0 {
		t.Fatalf("disabled provider should yield no issues, got %d", len(issues))
	}

	// A bare '*' exclusion is whole-credential disable, not a route conflict.
	cfg = diagFixtureConfig()
	cfg.OpenAICompatibility[0].APIKeyEntries[0].ExcludedModels = []string{"*"}
	codes := issueCodes(diagnoseOpenAICompat(cfg))
	if codes["route_excluded"] != 0 {
		t.Fatalf("bare * must not be reported as route_excluded, got %v", codes)
	}
}

func TestDiagMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"gpt-5.4", "gpt-5.4", true},
		{"GPT-5.4", "gpt-5.4", true},
		{"gpt-*", "gpt-5.4", true},
		{"*-5.4", "gpt-5.4", true},
		{"gpt-*", "claude-3", false},
		{"", "gpt", false},
	}
	for _, tc := range cases {
		if got := diagMatchPattern(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("diagMatchPattern(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestCompatConfiguredModels_RouteNameGrouping(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "acme",
			BaseURL: "https://acme.example/v1",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
				APIKey: "key-a",
				Models: []config.OpenAICompatibilityModel{
					{Name: "hidden-upstream", Alias: "route-a"},
					{Name: "kept-upstream", Alias: "route-b", Fork: true},
					{Name: "plain-model"},
				},
			}},
		}},
	}

	got := compatConfiguredModels(cfg, "acme", "", "https://acme.example/v1", "key-a")
	want := map[string]bool{"route-a": true, "route-b": true, "kept-upstream": true, "plain-model": true}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want keys %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected route id %q in %v", id, got)
		}
	}
	for _, id := range got {
		if id == "hidden-upstream" {
			t.Fatalf("hidden upstream name leaked into rows: %v", got)
		}
	}
}
