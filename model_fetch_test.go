package main

import "testing"

func TestOpenCodeGoFallbackCatalogIncludesCurrentModels(t *testing.T) {
	models := openCodeGoModels()
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	for _, want := range []string{"glm-5.2", "kimi-k2.7-code", "qwen3.7-max", "qwen3.7-plus", "hy3-preview"} {
		if !ids[want] {
			t.Fatalf("fallback catalog missing %q", want)
		}
	}
}

func TestEnrichOpenCodeGoModel(t *testing.T) {
	model := enrichOpenCodeGoModel(ModelInfo{ID: "glm-5.2"})
	if model.Name != "GLM-5.2" {
		t.Fatalf("name = %q, want GLM-5.2", model.Name)
	}
	if model.ContextSize != 128_000 {
		t.Fatalf("context size = %d, want 128000", model.ContextSize)
	}

	unknown := enrichOpenCodeGoModel(ModelInfo{ID: "new-model"})
	if unknown.Name != "New Model" {
		t.Fatalf("unknown name = %q, want New Model", unknown.Name)
	}
}
