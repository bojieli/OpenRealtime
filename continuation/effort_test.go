package continuation

import "testing"

func TestAnEffortIsANameOrANumberOfThinkingTokens(t *testing.T) {
	for _, name := range []string{"minimal", "low", "medium", "high"} {
		effort, err := ParseEffort(name)
		if err != nil {
			t.Fatalf("%q rejected: %v", name, err)
		}
		if !effort.Named() {
			t.Fatalf("%q did not read as a name", name)
		}
		if _, numeric := effort.Budget(); numeric {
			t.Fatalf("%q also read as a number", name)
		}
	}
	for _, text := range []string{"0", "512", "1024"} {
		effort, err := ParseEffort(text)
		if err != nil {
			t.Fatalf("%q rejected: %v", text, err)
		}
		if effort.Named() {
			t.Fatalf("%q read as a name", text)
		}
		if _, numeric := effort.Budget(); !numeric {
			t.Fatalf("%q did not read as a number", text)
		}
	}
	// A negative budget is not a smaller one, and neither is a word.
	for _, bad := range []string{"", "  ", "-1", "lots", "512tokens", "5.5"} {
		if _, err := ParseEffort(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if _, numeric := Effort("high").Budget(); numeric {
		t.Fatal("a name read as a budget")
	}
}
