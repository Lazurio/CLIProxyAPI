package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestCodexClientModelsResponse_VisibleProvidersHidesOtherHarnesses(t *testing.T) {
	const (
		codexClient  = "codex-visible-providers-codex-client"
		claudeClient = "codex-visible-providers-claude-client"
		codexModel   = "codex-visible-providers-gpt"
		claudeModel  = "codex-visible-providers-claude"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(codexClient, "codex", []*registry.ModelInfo{{ID: codexModel}})
	modelRegistry.RegisterClient(claudeClient, "claude", []*registry.ModelInfo{{ID: claudeModel}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(codexClient)
		modelRegistry.UnregisterClient(claudeClient)
	})

	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	handler := NewOpenAIAPIHandler(base)

	for _, tt := range []struct {
		name          string
		visible       []string
		claudeVisible bool
	}{
		{name: "unset shows every model", visible: nil, claudeVisible: true},
		{name: "codex only hides claude", visible: []string{" Codex "}, claudeVisible: false},
		{name: "both providers show both", visible: []string{"codex", "claude"}, claudeVisible: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base.Cfg.Client.Codex.VisibleProviders = tt.visible
			models, ok := handler.codexClientModelsResponse("0.164.0")["models"].([]map[string]any)
			if !ok {
				t.Fatal("models missing from Codex client catalog")
			}
			entries := make(map[string]map[string]any, len(models))
			for _, entry := range models {
				slug, _ := entry["slug"].(string)
				entries[slug] = entry
			}
			codexEntry, claudeEntry := entries[codexModel], entries[claudeModel]
			if codexEntry == nil || claudeEntry == nil {
				t.Fatalf("both models must stay in the catalog so explicit names still run; got codex=%v claude=%v", codexEntry != nil, claudeEntry != nil)
			}
			if codexEntry["visibility"] == "hide" {
				t.Fatalf("codex model hidden with visible-providers %v", tt.visible)
			}
			if hidden := claudeEntry["visibility"] == "hide"; hidden == tt.claudeVisible {
				t.Fatalf("claude model hidden=%v with visible-providers %v, want hidden=%v", hidden, tt.visible, !tt.claudeVisible)
			}
		})
	}
}
