package scenarioconversation

import (
	"strings"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestInvocationForSettingsComposesTheExactProfileOwnedContinuationInstruction(t *testing.T) {
	const policy = "Use only evidence already received; preserve deferred wording."
	config := PluginConfig{
		Gate: perception.DefaultGateConfig(), MaxOutputTokens: 128,
		ContinuationInstruction: policy,
	}
	settings := legacy.Settings{
		Instruction: "Answer briefly.", Modalities: []string{"text"}, Gate: config.Gate,
	}
	invocation, err := (&session{config: config}).invocationForSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Instruction != settings.Instruction+"\n\n"+policy || invocation.MaxOutputTokens != 128 {
		t.Fatalf("composed invocation = %+v", invocation)
	}
	settings.Instruction = "mutated after composition"
	if invocation.Instruction != "Answer briefly.\n\n"+policy {
		t.Fatalf("invocation retained mutable settings: %q", invocation.Instruction)
	}
}

func TestInvocationForSettingsPreservesAnExactClientInstructionWhenNoProfilePolicyIsSelected(t *testing.T) {
	config := PluginConfig{Gate: perception.DefaultGateConfig(), MaxOutputTokens: 128}
	settings := legacy.Settings{
		Instruction: "  Preserve this exact spacing.  ", Modalities: []string{"text"}, Gate: config.Gate,
	}
	invocation, err := (&session{config: config}).invocationForSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Instruction != settings.Instruction {
		t.Fatalf("instruction = %q, want exact %q", invocation.Instruction, settings.Instruction)
	}
}

func TestInvocationForSettingsRefusesAComposedInstructionPastTheAdapterBound(t *testing.T) {
	const policy = "profile policy"
	config := PluginConfig{
		Gate: perception.DefaultGateConfig(), MaxOutputTokens: 128,
		ContinuationInstruction: policy,
	}
	exact := maximumAdapterTextBytes - len("\n\n") - len(policy)
	for _, testCase := range []struct {
		name    string
		length  int
		wantErr bool
	}{
		{name: "at bound", length: exact},
		{name: "one over bound", length: exact + 1, wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settings := legacy.Settings{
				Instruction: strings.Repeat("x", testCase.length), Modalities: []string{"text"}, Gate: config.Gate,
			}
			invocation, err := (&session{config: config}).invocationForSettings(settings)
			if testCase.wantErr {
				if err == nil || !strings.Contains(err.Error(), "composed instruction") {
					t.Fatalf("oversized composed instruction error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(invocation.Instruction) != maximumAdapterTextBytes {
				t.Fatalf("composed instruction bytes = %d, want %d", len(invocation.Instruction), maximumAdapterTextBytes)
			}
		})
	}
}
