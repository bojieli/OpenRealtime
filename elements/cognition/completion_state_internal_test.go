package cognition

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A completion may carry opaque provider state that a later turn hands back to
// the same provider. validateCompletion is what keeps that state readable and
// attributable: it must arrive with its type, be parseable, and match the type
// the descriptor declared. Two of its three refusals had no coverage, so state
// could have been recorded with no type -- unreadable on the way back -- or
// under a type the provider never declared.
func completionDescriptor(nativeState string) continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "test-model", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, NativeStateType: nativeState,
	}
}

func TestCompletionProviderStateMustBeTypedParseableAndDeclared(t *testing.T) {
	t.Parallel()
	// No state at all is the ordinary case and stays valid.
	if err := validateCompletion(continuation.Completion{StopReason: "stop"}, completionDescriptor("")); err != nil {
		t.Fatalf("completion without provider state = %v, want accepted", err)
	}
	paired := continuation.Completion{
		StopReason:        "stop",
		ProviderStateType: "vendor/state.v1",
		ProviderState:     json.RawMessage(`{"cursor":7}`),
	}
	if err := validateCompletion(paired, completionDescriptor("vendor/state.v1")); err != nil {
		t.Fatalf("declared provider state = %v, want accepted", err)
	}
	if err := validateCompletion(paired, completionDescriptor("")); err != nil {
		t.Fatalf("provider state with no declared native type = %v, want accepted", err)
	}

	for _, test := range []struct {
		name       string
		completion continuation.Completion
		descriptor continuation.Descriptor
		want       string
	}{
		{
			name: "state without its type",
			completion: continuation.Completion{
				StopReason: "stop", ProviderState: json.RawMessage(`{"cursor":7}`),
			},
			descriptor: completionDescriptor(""),
			want:       "must be supplied together",
		},
		{
			name: "type without its state",
			completion: continuation.Completion{
				StopReason: "stop", ProviderStateType: "vendor/state.v1",
			},
			descriptor: completionDescriptor(""),
			want:       "must be supplied together",
		},
		{
			name: "state is not parseable",
			completion: continuation.Completion{
				StopReason:        "stop",
				ProviderStateType: "vendor/state.v1",
				ProviderState:     json.RawMessage(`{"cursor":`),
			},
			descriptor: completionDescriptor("vendor/state.v1"),
			want:       "must be supplied together",
		},
		{
			// State recorded under a type the provider never declared cannot
			// be handed back to it.
			name:       "state type is not the type the descriptor declared",
			completion: paired,
			descriptor: completionDescriptor("vendor/state.v2"),
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
