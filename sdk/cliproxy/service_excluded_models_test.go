package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"gemini": {"gemini-2.5-pro"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-gemini",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-flash",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("gemini")
	if len(models) == 0 {
		t.Fatal("expected gemini models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "gemini-2.5-flash") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "gemini-2.5-pro") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_MetaOAuthAliasAndExcludedModels(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"meta": {"muse-spark-1.1"},
			},
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"meta": {{Name: "muse-spark-1.3", Alias: "muse-latest"}},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-meta-oauth",
		Provider: "meta",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"api_key":   "LLM|minted",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) == 0 {
		t.Fatal("expected meta models to be registered")
	}

	seenLatest := false
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		switch {
		case strings.EqualFold(modelID, "muse-spark-1.1"):
			t.Fatalf("expected model %q to be excluded by oauth-excluded-models", modelID)
		case strings.EqualFold(modelID, "muse-spark-1.3"):
			t.Fatalf("expected model %q to be renamed by oauth-model-alias", modelID)
		case strings.EqualFold(modelID, "muse-latest"):
			seenLatest = true
		}
	}
	if !seenLatest {
		t.Fatal("expected oauth-model-alias to expose muse-latest")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityImageModelType(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "images",
					BaseURL: "https://example.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{Name: "upstream-image", Alias: "compat-image", Image: true},
						{Name: "upstream-chat", Alias: "compat-chat"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-image",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "images",
			"provider_key": "images",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	var imageModel *internalregistry.ModelInfo
	var chatModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "compat-image":
			imageModel = model
		case "compat-chat":
			chatModel = model
		}
	}
	if imageModel == nil {
		t.Fatal("expected compat-image to be registered")
	}
	if imageModel.Type != internalregistry.OpenAIImageModelType {
		t.Fatalf("image model type = %q, want %q", imageModel.Type, internalregistry.OpenAIImageModelType)
	}
	if imageModel.Thinking != nil {
		t.Fatalf("image model thinking = %+v, want nil", imageModel.Thinking)
	}
	if chatModel == nil {
		t.Fatal("expected compat-chat to be registered")
	}
	if chatModel.Type != "openai-compatibility" {
		t.Fatalf("chat model type = %q, want openai-compatibility", chatModel.Type)
	}
	if chatModel.Thinking == nil {
		t.Fatal("expected chat model to keep default thinking support")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityInputModalities(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "mimo",
					BaseURL: "https://example.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{
							Name:             "mimo-v2.5-pro",
							Alias:            "mimo-v2.5-pro",
							InputModalities:  []string{"text", "image"},
							OutputModalities: []string{"text"},
						},
						{Name: "upstream-image", Alias: "compat-image", Image: true},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-modalities",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "mimo",
			"provider_key": "mimo",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	var visionModel *internalregistry.ModelInfo
	var imageEndpointModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "mimo-v2.5-pro":
			visionModel = model
		case "compat-image":
			imageEndpointModel = model
		}
	}
	if visionModel == nil {
		t.Fatal("expected mimo-v2.5-pro to be registered")
	}
	if visionModel.Type != "openai-compatibility" {
		t.Fatalf("vision model type = %q, want openai-compatibility", visionModel.Type)
	}
	if got := strings.Join(visionModel.SupportedInputModalities, ","); got != "text,image" {
		t.Fatalf("SupportedInputModalities = %q, want text,image", got)
	}
	if got := strings.Join(visionModel.SupportedOutputModalities, ","); got != "text" {
		t.Fatalf("SupportedOutputModalities = %q, want text", got)
	}
	if imageEndpointModel == nil {
		t.Fatal("expected compat-image to be registered")
	}
	if imageEndpointModel.Type != internalregistry.OpenAIImageModelType {
		t.Fatalf("image endpoint model type = %q, want %q", imageEndpointModel.Type, internalregistry.OpenAIImageModelType)
	}
	if len(imageEndpointModel.SupportedInputModalities) != 0 {
		t.Fatalf("image endpoint model should not inherit chat input modalities: %+v", imageEndpointModel.SupportedInputModalities)
	}
}

func TestRegisterModelsForAuth_AntigravityUsesAuthoritativeUpstreamModels(t *testing.T) {
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityModelsPath {
			t.Fatalf("path = %q, want %s", r.URL.Path, antigravityModelsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		if got := string(body); got != `{"project":"project-1"}` {
			t.Fatalf("request body = %s, want project payload", got)
		}
		sawFetch = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
				"models": {
					"gemini-3.1-flash-lite": {
						"displayName": "Gemini 3.1 Flash Lite",
						"maxTokens": 1,
						"maxOutputTokens": 2
					},
					"gemini-3.6-flash-low": {
						"displayName": "Gemini 3.6 Flash",
						"maxTokens": 1048576,
						"maxOutputTokens": 65536
					}
				},
				"webSearchModelIds": ["gemini-3.6-flash-low"]
			}`))
	}))
	defer server.Close()

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-antigravity-fetch-models",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
		},
	}

	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)
	service.WaitAntigravityProbes()
	if !sawFetch {
		t.Fatal("expected fetchAvailableModels request")
	}

	models := registry.GetModelsForClient(auth.ID)

	if len(models) != 2 {
		t.Fatalf("registered models = %d, want exactly upstream model count 2: %#v", len(models), models)
	}

	var fetchedLiteModel, fetchedLowModel, staticOnlyModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "gemini-3.1-flash-lite":
			fetchedLiteModel = model
		case "gemini-3.6-flash-low":
			fetchedLowModel = model
		case "gpt-oss-120b-medium":
			staticOnlyModel = model
		}
	}
	if fetchedLiteModel == nil {
		t.Fatal("expected gemini-3.1-flash-lite to be registered")
	}
	if fetchedLiteModel.SupportsWebSearch {
		t.Fatal("expected gemini-3.1-flash-lite to be absent from web search model ids")
	}
	if fetchedLiteModel.ContextLength != 1 || fetchedLiteModel.MaxCompletionTokens != 2 {
		t.Fatalf("upstream token limits should win, got=%#v", fetchedLiteModel)
	}
	if fetchedLiteModel.Thinking != nil {
		t.Fatalf("static metadata should not be supplemented, got thinking=%#v", fetchedLiteModel.Thinking)
	}
	if fetchedLowModel == nil {
		t.Fatal("expected upstream gemini-3.6-flash-low to be registered")
	}
	if !fetchedLowModel.SupportsWebSearch {
		t.Fatal("expected gemini-3.6-flash-low to support web search")
	}
	if fetchedLowModel.ContextLength != 1048576 || fetchedLowModel.MaxCompletionTokens != 65536 {
		t.Fatalf("upstream model token metadata = %#v", fetchedLowModel)
	}
	if staticOnlyModel != nil {
		t.Fatalf("static-only model should not be registered: %#v", staticOnlyModel)
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityHonorsExcludedModels(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "ciallo",
					BaseURL: "https://example.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{Name: "gpt-5.4", Alias: "gpt-5.4"},
						{Name: "grok-4.5", Alias: "grok-4.5"},
						{Name: "deepseek-v4-flash", Alias: "deepseek-v4-flash"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-excluded",
		Provider: "openai-compatible-ciallo",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "api_key",
			"compat_name":     "ciallo",
			"provider_key":    "openai-compatible-ciallo",
			"excluded_models": "gpt-5.4,grok-4.5",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	seen := map[string]bool{}
	for _, model := range models {
		if model == nil {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	if seen["gpt-5.4"] {
		t.Fatal("expected gpt-5.4 to be excluded from openai-compat registration")
	}
	if seen["grok-4.5"] {
		t.Fatal("expected grok-4.5 to be excluded from openai-compat registration")
	}
	if !seen["deepseek-v4-flash"] {
		t.Fatal("expected non-excluded model deepseek-v4-flash to remain registered")
	}
}
