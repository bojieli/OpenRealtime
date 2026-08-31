package element_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// The reaction contract is what makes a graph's trigger relation inspectable:
// it says which ports wake this element, which it merely samples, which
// interrupt it, and which carry its outcome. Two refusals were unexercised,
// and the direction one is the load-bearing half -- a reaction that named an
// output as its trigger would describe an element woken by its own result.
func reactionDescriptor(reaction element.Reaction) element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Reaction",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "state", Direction: element.Input, Type: valueType, Cardinality: element.One, DefaultDepth: 1},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, DefaultDepth: 1},
		},
		Reaction: reaction,
	}
}

func TestReactionPortsMustExistFaceTheRightWayAndHaveOneRole(t *testing.T) {
	t.Parallel()
	sound := reactionDescriptor(element.Reaction{
		Triggers: []string{"in"}, SampledState: []string{"state"},
		Outcomes: []string{"out"}, MaxConcurrency: 1,
	})
	if err := sound.Validate(); err != nil {
		t.Fatalf("well-formed reaction = %v, want accepted", err)
	}

	for _, test := range []struct {
		name     string
		reaction element.Reaction
		want     string
	}{
		{
			name:     "negative concurrency",
			reaction: element.Reaction{Triggers: []string{"in"}, MaxConcurrency: -1},
			want:     "negative max concurrency",
		},
		{
			name:     "trigger names a port that does not exist",
			reaction: element.Reaction{Triggers: []string{"absent"}, MaxConcurrency: 1},
			want:     "unknown trigger port",
		},
		{
			// An element woken by its own result.
			name:     "trigger names an output port",
			reaction: element.Reaction{Triggers: []string{"out"}, MaxConcurrency: 1},
			want:     `trigger port "out" is output, want input`,
		},
		{
			name: "outcome names an input port",
			reaction: element.Reaction{
				Triggers: []string{"in"}, Outcomes: []string{"state"}, MaxConcurrency: 1,
			},
			want: `outcome port "state" is input, want output`,
		},
		{
			name: "interrupt names an output port",
			reaction: element.Reaction{
				Triggers: []string{"in"}, Interrupts: []string{"out"}, MaxConcurrency: 1,
			},
			want: `interrupt port "out" is output, want input`,
		},
		{
			// One port, two roles: nothing can say whether an arrival on it
			// wakes the element or is merely read when something else does.
			name: "one port is both a trigger and sampled state",
			reaction: element.Reaction{
				Triggers: []string{"in"}, SampledState: []string{"in"}, MaxConcurrency: 1,
			},
			want: "is both trigger and sampled state",
		},
		{
			name: "one port is both a trigger and an interrupt",
			reaction: element.Reaction{
				Triggers: []string{"in"}, Interrupts: []string{"in"}, MaxConcurrency: 1,
			},
			want: "is both trigger and interrupt",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := reactionDescriptor(test.reaction).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("reaction error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
