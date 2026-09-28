package governance

import "testing"

func TestCatalogSkillsFromCategory(t *testing.T) {
	caps, _, outputs := catalogSkills(categoryTokens("Reasoning, long context", nil), 200000)
	for _, want := range []string{"reasoning", "analysis", "mathematics", "architecture", "coding", "long_context", "context_understanding"} {
		if !containsFold(caps, want) {
			t.Errorf("Reasoning, long context missing %s in %v", want, caps)
		}
	}
	if !containsFold(outputs, "text") {
		t.Errorf("outputs = %v, want text", outputs)
	}

	_, _, embedOut := catalogSkills(categoryTokens("Embeddings", nil), 8000)
	if containsFold(embedOut, "text") {
		t.Errorf("embeddings outputs = %v, want no text", embedOut)
	}

	general, _, _ := catalogSkills(categoryTokens("General purpose", nil), 128000)
	for _, want := range []string{"long_context", "context_understanding", "general_knowledge"} {
		if !containsFold(general, want) {
			t.Errorf("128k general purpose missing %s in %v", want, general)
		}
	}
}
