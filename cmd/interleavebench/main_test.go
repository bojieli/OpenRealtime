package main

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

func TestParseEffort(t *testing.T) {
	t.Parallel()
	effort, err := parseEffort(" HIGH ")
	if err != nil || effort != continuation.EffortHigh {
		t.Fatalf("got effort=%q err=%v", effort, err)
	}
	if _, err := parseEffort("maximum"); err == nil {
		t.Fatal("expected unsupported effort to fail")
	}
}

func TestVLLMRequiresExactModel(t *testing.T) {
	t.Parallel()
	if _, err := makeProvider("vllm", "", "http://127.0.0.1:8000/v1", "fast", continuation.EffortMinimal); err == nil {
		t.Fatal("expected missing vLLM model to fail")
	}
}
