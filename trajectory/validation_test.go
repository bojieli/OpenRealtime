package trajectory

import (
	"encoding/json"
	"strings"
	"testing"
)

// validateCommon and validateToolCall run on every item that reaches the
// canonical record. Their refusals had no coverage: the store's tests append
// well-formed items, so each guard could be deleted without a failure. The
// record is what authority, replay, and audit are all reconstructed from, so a
// malformed item admitted here is not a local problem.

func TestCanonicalItemsRequireIdentityAndProducerAttribution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		item Item
		want string
	}{
		{
			name: "no item ID",
			item: Item{ID: "  ", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "x"},
			want: "item ID is required",
		},
		{
			name: "no producer phase",
			item: Item{ID: "one", Kind: KindObservation, Content: "x"},
			want: "producer phase is required",
		},
		{
			// Provider state without its type is unreadable later: replay
			// cannot know what it is looking at, so it must not be recorded.
			name: "provider state without its type",
			item: Item{
				ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser},
				Content: "x", ProviderState: json.RawMessage(`{"a":1}`),
			},
			want: "provider state type is required",
		},
		{
			name: "provider state type without parseable state",
			item: Item{
				ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser},
				Content: "x", ProviderStateType: "vendor/state", ProviderState: json.RawMessage(`{`),
			},
			want: "provider state must be valid JSON",
		},
		{
			name: "provider state type with no state at all",
			item: Item{
				ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser},
				Content: "x", ProviderStateType: "vendor/state",
			},
			want: "provider state must be valid JSON",
		},
		{
			name: "event metadata missing its channel",
			item: Item{
				ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "x",
				Event: &EventMetadata{EventID: "e1", Type: "t", Source: "s"},
			},
			want: "requires event ID, type, source, and channel",
		},
		{
			name: "event metadata on a kind that cannot carry it",
			item: Item{
				ID: "one", Kind: KindInstruction, Producer: Producer{Phase: PhaseUser}, Content: "x",
				Event: &EventMetadata{EventID: "e1", Type: "t", Source: "s", Channel: "c"},
			},
			want: "event metadata is not valid on",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := NewStore().Append(test.item)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestCanonicalToolCallsRequireIdentityAndOneJSONObject(t *testing.T) {
	t.Parallel()
	call := func(value ToolCall) Item {
		return Item{
			ID: "call-item", Kind: KindToolCall, InvocationID: "inv",
			Producer: Producer{Phase: PhaseSlow, Provider: "p", Model: "m"}, ToolCall: &value,
		}
	}
	for _, test := range []struct {
		name string
		item Item
		want string
	}{
		{
			name: "no call ID",
			item: call(ToolCall{Name: "calendar.read", Arguments: json.RawMessage(`{}`)}),
			want: "ID and name are required",
		},
		{
			name: "no name",
			item: call(ToolCall{CallID: "call-1", Arguments: json.RawMessage(`{}`)}),
			want: "ID and name are required",
		},
		{
			name: "no arguments",
			item: call(ToolCall{CallID: "call-1", Name: "calendar.read"}),
			want: "must be valid JSON",
		},
		{
			name: "truncated arguments",
			item: call(ToolCall{
				CallID: "call-1", Name: "calendar.read", Arguments: json.RawMessage(`{"day":`),
			}),
			want: "must be valid JSON",
		},
		{
			name: "arguments are valid JSON but not an object",
			item: call(ToolCall{
				CallID: "call-1", Name: "calendar.read", Arguments: json.RawMessage(`[1,2]`),
			}),
			want: "must be one JSON object",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := NewStore().Append(test.item)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
