package continuation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// A completion may carry opaque provider state that a later turn hands back to
// the same provider. validateCompletion keeps that state readable and
// attributable: it must arrive with its type, parse, and match the type the
// descriptor declared. Two of its three refusals had no coverage.
func completionStateDescriptor(nativeState string) Descriptor {
	return Descriptor{
		Provider: "test", Model: "test-model", Phase: trajectory.PhaseFast,
		Effort: EffortMinimal, NativeStateType: nativeState,
	}
}

func TestCompletionStateMustBeTypedParseableAndDeclared(t *testing.T) {
	t.Parallel()
	// Carrying no state at all is the ordinary case.
	if err := validateCompletion(Completion{StopReason: "stop"}, completionStateDescriptor("")); err != nil {
		t.Fatalf("completion without provider state = %v, want accepted", err)
	}
	paired := Completion{
		StopReason:        "stop",
		ProviderStateType: "vendor/state.v1",
		ProviderState:     json.RawMessage(`{"cursor":7}`),
	}
	if err := validateCompletion(paired, completionStateDescriptor("vendor/state.v1")); err != nil {
		t.Fatalf("declared provider state = %v, want accepted", err)
	}
	if err := validateCompletion(paired, completionStateDescriptor("")); err != nil {
		t.Fatalf("state with no declared native type = %v, want accepted", err)
	}

	for _, test := range []struct {
		name       string
		completion Completion
		descriptor Descriptor
		want       string
	}{
		{
			// State with no type is unreadable on the way back: nothing knows
			// what it is looking at.
			name: "state without its type",
			completion: Completion{
				StopReason: "stop", ProviderState: json.RawMessage(`{"cursor":7}`),
			},
			descriptor: completionStateDescriptor(""),
			want:       "must be supplied together",
		},
		{
			name: "type without its state",
			completion: Completion{
				StopReason: "stop", ProviderStateType: "vendor/state.v1",
			},
			descriptor: completionStateDescriptor(""),
			want:       "must be supplied together",
		},
		{
			name: "state does not parse",
			completion: Completion{
				StopReason:        "stop",
				ProviderStateType: "vendor/state.v1",
				ProviderState:     json.RawMessage(`{"cursor":`),
			},
			descriptor: completionStateDescriptor("vendor/state.v1"),
			want:       "must be supplied together",
		},
		{
			// State recorded under a type the provider never declared cannot
			// be handed back to it.
			name:       "state type is not the one the descriptor declared",
			completion: paired,
			descriptor: completionStateDescriptor("vendor/state.v2"),
			want:       "does not match descriptor",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateCompletion(test.completion, test.descriptor)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("completion error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
