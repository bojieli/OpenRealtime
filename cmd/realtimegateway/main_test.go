package main

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestParseSlowEffortKeepsDeliberativeProfiles(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"medium", "HIGH"} {
		if _, err := parseSlowEffort(value); err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
	}
	if _, err := parseSlowEffort(string(continuation.EffortMinimal)); err == nil {
		t.Fatal("minimal effort was accepted for the slow continuation")
	}
}

func TestMakeFastProviderProfiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		options   options
		key       string
		provider  string
		model     string
		wantError bool
	}{
		{name: "local reflex", options: options{fastProvider: "vllm", fastURL: "http://127.0.0.1:8000/v1"}, provider: "vllm", model: "qwen-fast"},
		{name: "hosted minimal", options: options{fastProvider: "gemini"}, key: "secret", provider: "google", model: "gemini-3.5-flash"},
		{name: "unknown", options: options{fastProvider: "router"}, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider, err := makeFastProvider(test.options, test.key)
			if (err != nil) != test.wantError {
				t.Fatalf("makeFastProvider() error=%v, wantError=%v", err, test.wantError)
			}
			if test.wantError {
				return
			}
			descriptor := provider.Descriptor()
			if descriptor.Provider != test.provider || descriptor.Model != test.model || descriptor.Phase != trajectory.PhaseFast ||
				descriptor.Effort != continuation.EffortMinimal || descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
				t.Fatalf("unexpected fast descriptor: %#v", descriptor)
			}
		})
	}
}
