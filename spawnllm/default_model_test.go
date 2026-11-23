package spawnllm

import "testing"

func TestProviders_HaveNoDefaultModel(t *testing.T) {
	http := NewHTTPProviderWithOptions("k", "https://example.invalid/v1", "")
	if got := http.GetDefaultModel(); got != "" {
		t.Errorf("HTTPProvider.GetDefaultModel() = %q, want empty: the model comes from configuration", got)
	}
	if http.Delegate() == nil {
		t.Error("HTTPProvider.Delegate() = nil, want the openai_compat provider it wraps")
	}
	if got := NewUnconfiguredProvider().GetDefaultModel(); got != "" {
		t.Errorf("UnconfiguredProvider.GetDefaultModel() = %q, want empty", got)
	}
}
