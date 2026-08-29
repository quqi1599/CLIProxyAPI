package cliproxy

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestOpenAICompatibilityThinkingSupportZhipuGLM53(t *testing.T) {
	tests := []struct {
		name   string
		compat config.OpenAICompatibility
		model  config.OpenAICompatibilityModel
	}{
		{
			name:   "explicit zhipu kind",
			compat: config.OpenAICompatibility{Kind: "zhipu"},
			model:  config.OpenAICompatibilityModel{Name: "glm-5.3"},
		},
		{
			name:   "GLM-5.3-Flash with explicit zhipu kind",
			compat: config.OpenAICompatibility{Kind: "zhipu"},
			model:  config.OpenAICompatibilityModel{Name: "glm-5.3-flash"},
		},
		{
			name:   "zhipu inferred from base URL",
			compat: config.OpenAICompatibility{BaseURL: "https://api.z.ai/api/coding/paas/v4"},
			model: config.OpenAICompatibilityModel{
				Name: "GLM-5.3",
				Thinking: &registry.ThinkingSupport{
					Min:            1,
					Max:            999,
					ZeroAllowed:    true,
					DynamicAllowed: true,
					Levels:         []string{"none", "medium"},
				},
			},
		},
		{
			name:   "GLM-5.3-Flash inferred from base URL",
			compat: config.OpenAICompatibility{BaseURL: "https://api.z.ai/api/coding/paas/v4"},
			model:  config.OpenAICompatibilityModel{Name: "GLM-5.3-FLASH"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			support := openAICompatibilityThinkingSupport(&test.compat, test.model)
			if support == nil {
				t.Fatal("thinking support is nil")
			}
			if support.Min != 0 || support.Max != 0 || support.ZeroAllowed || support.DynamicAllowed || !slices.Equal(support.Levels, []string{"low", "high", "max"}) {
				t.Fatalf("thinking support = %+v, want forced low/high/max levels", support)
			}
		})
	}
}

func TestOpenAICompatibilityThinkingSupportKeepsGLM52Defaults(t *testing.T) {
	compat := config.OpenAICompatibility{Kind: "zhipu"}
	support := openAICompatibilityThinkingSupport(&compat, config.OpenAICompatibilityModel{Name: "glm-5.2"})
	if support == nil || !slices.Equal(support.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("GLM-5.2 thinking support = %+v, want generic defaults", support)
	}
}
