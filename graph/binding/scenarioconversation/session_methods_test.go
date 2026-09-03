package scenarioconversation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
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

func TestInvocationUsesDeploymentToolPolicyWithoutProjectingItToProviders(t *testing.T) {
	parameters := json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}}}`)
	tools, err := normalizeToolDeclarations([]ToolDeclaration{{
		Name: "track_order", Description: "track an order", Parameters: parameters,
		ArgumentNormalizers: []legacyaction.ToolArgumentNormalizer{{
			Argument: "order_id", Normalizer: legacyaction.ToolParameterCompactASCIIAlphanumericV1,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	config := PluginConfig{
		Gate: perception.DefaultGateConfig(), MaxOutputTokens: 128, Tools: tools,
		Model: ModelPlugin{Descriptor: continuation.Descriptor{Phase: trajectory.PhaseFast}},
	}
	settings := legacy.Settings{
		Instruction: "Answer briefly.", Modalities: []string{"text"}, Gate: config.Gate,
		Tools: []legacyaction.ToolSpec{{
			Name: "track_order", Description: "track an order", Parameters: parameters,
		}},
	}
	withoutClientMetadata, err := (&session{config: config}).invocationForSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.Tools[0].ArgumentNormalizers = []legacyaction.ToolArgumentNormalizer{{
		Argument: "order_id", Normalizer: "malicious-client-override",
	}}
	withClientMetadata, err := (&session{config: config}).invocationForSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutClientMetadata, withClientMetadata) {
		t.Fatalf("client-private metadata changed provider invocation:\nwithout: %+v\nwith: %+v",
			withoutClientMetadata, withClientMetadata)
	}
	if len(config.Tools[0].ArgumentNormalizers) != 1 ||
		config.Tools[0].ArgumentNormalizers[0].Normalizer !=
			legacyaction.ToolParameterCompactASCIIAlphanumericV1 {
		t.Fatalf("client metadata replaced deployment action policy: %+v", config.Tools[0])
	}
	encoded, err := json.Marshal(withClientMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "argument_normalizers") ||
		strings.Contains(string(encoded), legacyaction.ToolParameterCompactASCIIAlphanumericV1) ||
		strings.Contains(string(encoded), "malicious-client-override") {
		t.Fatalf("provider invocation exposed private action policy: %s", encoded)
	}
}
