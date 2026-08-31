package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

// validateInvocation bounds what a graph may put in front of the generation
// policy. Nine of its thirteen refusals had no coverage. They are bounds, not
// shapes: an unbounded instruction, an unbounded tool schema, or unbounded
// aggregate metadata all become one very large provider request built from
// graph values, and the point of validating here is that the bound is enforced
// before the request is assembled rather than by whatever fails downstream.
func validPolicyInvocation() continuation.Invocation {
	return continuation.Invocation{
		Instruction:  "decide whether to speak",
		Capabilities: []continuation.Capability{{Name: "vision", Description: "sees frames"}},
		Tools: []continuation.ToolDefinition{{
			Name: "lookup", Description: "look something up",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
}

func TestPolicyInvocationBoundsEveryCallerSuppliedField(t *testing.T) {
	t.Parallel()
	if err := validateInvocation(validPolicyInvocation()); err != nil {
		t.Fatalf("well-formed invocation = %v, want accepted", err)
	}
	for _, test := range []struct {
		name string
		edit func(*continuation.Invocation)
		want string
	}{
		{
			name: "no instruction",
			edit: func(value *continuation.Invocation) { value.Instruction = "  " },
			want: "instruction is required",
		},
		{
			name: "instruction beyond its byte bound",
			edit: func(value *continuation.Invocation) {
				value.Instruction = strings.Repeat("a", maximumPolicyInstructionBytes+1)
			},
			want: "no larger than",
		},
		{
			name: "instruction is not valid UTF-8",
			edit: func(value *continuation.Invocation) { value.Instruction = "decide \xff" },
			want: "valid UTF-8",
		},
		{
			// Source revision is commit evidence, not something a caller may
			// assert: accepting one here would let a graph value stand in for
			// the revision the trajectory actually committed.
			name: "caller asserts a source revision",
			edit: func(value *continuation.Invocation) { value.SourceRevision = 7 },
			want: "derived from commit evidence",
		},
		{
			name: "negative output bound",
			edit: func(value *continuation.Invocation) { value.MaxOutputTokens = -1 },
			want: "must be between 0 and",
		},
		{
			name: "output bound beyond its maximum",
			edit: func(value *continuation.Invocation) {
				value.MaxOutputTokens = maximumPolicyOutputTokens + 1
			},
			want: "must be between 0 and",
		},
		{
			// The two effort failures are different and must stay
			// distinguishable: one is not a level at all, the other is the
			// right level spelled in a form the lock would not reproduce.
			name: "effort is not a known level",
			edit: func(value *continuation.Invocation) {
				value.Effort = continuation.Effort("exhaustive")
			},
			want: "reasoning effort must be minimal, low, medium, high",
		},
		{
			name: "effort is a known level in a non-canonical spelling",
			edit: func(value *continuation.Invocation) {
				value.Effort = continuation.Effort("MINIMAL")
			},
			want: "must already be canonical",
		},
		{
			name: "more capabilities than the entry bound",
			edit: func(value *continuation.Invocation) {
				value.Capabilities = make([]continuation.Capability, maximumPolicyEntries+1)
			},
			want: "more than",
		},
		{
			name: "more tools than the entry bound",
			edit: func(value *continuation.Invocation) {
				value.Tools = make([]continuation.ToolDefinition, maximumPolicyEntries+1)
			},
			want: "more than",
		},
		{
			name: "capability description is not valid UTF-8",
			edit: func(value *continuation.Invocation) {
				value.Capabilities[0].Description = "sees \xff"
			},
			want: "capability 0 description is not valid UTF-8",
		},
		{
			name: "tool description is not valid UTF-8",
			edit: func(value *continuation.Invocation) {
				value.Tools[0].Description = "look up \xff"
			},
			want: "tool 0 description is not valid UTF-8",
		},
		{
			name: "tool parameters are absent",
			edit: func(value *continuation.Invocation) { value.Tools[0].Parameters = nil },
			want: "tool 0 parameters must be valid JSON",
		},
		{
			name: "tool parameters are not valid JSON",
			edit: func(value *continuation.Invocation) {
				value.Tools[0].Parameters = json.RawMessage(`{"type":`)
			},
			want: "tool 0 parameters must be valid JSON",
		},
		{
			name: "tool parameters beyond their byte bound",
			edit: func(value *continuation.Invocation) {
				filler := strings.Repeat("a", maximumPolicyParametersBytes)
				value.Tools[0].Parameters = json.RawMessage(`{"d":"` + filler + `"}`)
			},
			want: "tool 0 parameters must be valid JSON",
		},
		{
			// Each entry can be inside its own bound while the aggregate is
			// not, which is the case a per-field check alone would miss.
			name: "aggregate metadata exceeds its bound",
			edit: func(value *continuation.Invocation) {
				half := strings.Repeat("d", maximumPolicyMetadataBytes/2+1)
				value.Capabilities = []continuation.Capability{
					{Name: "vision", Description: half},
					{Name: "hearing", Description: half},
				}
			},
			want: "metadata exceeds",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			invocation := validPolicyInvocation()
			test.edit(&invocation)
			err := validateInvocation(invocation)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invocation error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
