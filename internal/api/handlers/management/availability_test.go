package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAggregateAvailabilityGroupsCountsOnlyModelsWithTraffic(t *testing.T) {
	items := []coreusage.AvailabilitySnapshot{{
		Key:        coreusage.AvailabilityKey{Provider: "xai", Model: "grok-4"},
		Total:      1,
		Success:    1,
		HasTraffic: true,
	}}
	for i := 0; i < 38; i++ {
		items = append(items, coreusage.AvailabilitySnapshot{
			Key: coreusage.AvailabilityKey{Provider: "xai", Model: "idle-model"},
		})
	}

	groups := aggregateAvailabilityGroups(items)
	if len(groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(groups))
	}
	if groups[0].Models != 1 {
		t.Fatalf("models with traffic = %d, want 1", groups[0].Models)
	}
	if groups[0].IdleModels != 38 {
		t.Fatalf("idle models = %d, want 38", groups[0].IdleModels)
	}
}

func TestCompatConfiguredModelsKeepsPerKeyModelSetsSeparate(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "relay",
		BaseURL: "https://relay.example/v1",
		Models: []config.OpenAICompatibilityModel{
			{Name: "shared-default"},
		},
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{
			{APIKey: "key-1", Models: []config.OpenAICompatibilityModel{{Name: "deepseek"}}},
			{APIKey: "key-2", Models: []config.OpenAICompatibilityModel{{Name: "deepseek"}, {Name: "claude"}}},
			{APIKey: "key-3"},
		},
	}}}

	key1 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-1")
	if len(key1) != 1 || key1[0] != "deepseek" {
		t.Fatalf("key-1 models = %#v, want [deepseek]", key1)
	}

	key2 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-2")
	if len(key2) != 2 || key2[0] != "deepseek" || key2[1] != "claude" {
		t.Fatalf("key-2 models = %#v, want [deepseek claude]", key2)
	}

	key3 := compatConfiguredModels(cfg, "relay", "", "https://relay.example/v1", "key-3")
	if len(key3) != 1 || key3[0] != "shared-default" {
		t.Fatalf("key-3 models = %#v, want inherited [shared-default]", key3)
	}
}
