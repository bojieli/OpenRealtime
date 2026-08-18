package main

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
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
