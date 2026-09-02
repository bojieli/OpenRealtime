package element_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// Descriptor.Validate is the gate every element contract passes before it can
// be locked, mounted, or matched against Graph IR. Eleven of its sixteen
// refusals had no coverage. Together they are what stops an unlockable or
// self-contradictory contract from being digested and then trusted by identity
// for the rest of its life.
func baseDescriptor() element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Element",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, MaxConcurrency: 1},
	}
}

func TestDescriptorValidateRefusesUnlockableContracts(t *testing.T) {
	t.Parallel()
	if err := baseDescriptor().Validate(); err != nil {
		t.Fatalf("well-formed descriptor = %v, want accepted", err)
	}
	valueType := element.Event(element.Named("test.Value"))

	for _, test := range []struct {
		name string
		edit func(*element.Descriptor)
		want string
	}{
		{
			name: "descriptor format is not the one this build speaks",
			edit: func(d *element.Descriptor) { d.FormatVersion = element.DescriptorFormatVersion + 1 },
			want: "descriptor format",
		},
		{
			name: "no descriptor format at all",
			edit: func(d *element.Descriptor) { d.FormatVersion = 0 },
			want: "descriptor format",
		},
		{
			name: "element name is not a qualified name",
			edit: func(d *element.Descriptor) { d.Name = "element" },
			want: "invalid element name",
		},
		{
			// Revision is half of what a lock pins; zero is not a revision.
			name: "revision is zero",
			edit: func(d *element.Descriptor) { d.Revision = 0 },
			want: "revision must be positive",
		},
		{
			name: "generic is not a canonical variable",
			edit: func(d *element.Descriptor) { d.Generics = []string{"lowercase"} },
			want: "invalid generic",
		},
		{
			name: "a generic is declared twice",
			edit: func(d *element.Descriptor) { d.Generics = []string{"T", "T"} },
			want: "repeats generic",
		},
		{
			name: "port name is not canonical",
			edit: func(d *element.Descriptor) { d.Ports[0].Name = "In" },
			want: "invalid port name",
		},
		{
			name: "two ports share one name",
			edit: func(d *element.Descriptor) {
				d.Ports = append(d.Ports, element.Port{
					Name: "in", Direction: element.Output, Type: valueType,
					Cardinality: element.One, DefaultDepth: 1,
				})
			},
			want: "repeats port",
		},
		{
			name: "port direction is neither input nor output",
			edit: func(d *element.Descriptor) { d.Ports[0].Direction = element.Direction("both") },
			want: "invalid direction",
		},
		{
			// A singular port has one lane; requiring two connections on it
			// is a contract nothing can satisfy.
			name: "singular port requires more than one connection",
			edit: func(d *element.Descriptor) { d.Ports[0].MinConnections = 2 },
			want: "cannot require 2 connections",
		},
		{
			name: "variadic port requires a negative number of connections",
			edit: func(d *element.Descriptor) {
				d.Ports[0].Cardinality = element.Variadic
				d.Ports[0].MinConnections = -1
			},
			want: "negative minimum connections",
		},
		{
			name: "port cardinality is not declared",
			edit: func(d *element.Descriptor) { d.Ports[0].Cardinality = element.Cardinality("many") },
			want: "invalid cardinality",
		},
		{
			name: "port queue depth is negative",
			edit: func(d *element.Descriptor) { d.Ports[0].DefaultDepth = -1 },
			want: "negative default depth",
		},
		{
			// An element with no ports cannot be connected to anything, so it
			// can never run; the refusal is at authoring time rather than at
			// a mount that silently does nothing.
			name: "element declares no ports",
			edit: func(d *element.Descriptor) {
				d.Ports = nil
				d.Reaction = element.Reaction{}
			},
			want: "has no ports",
		},
		{
			name: "state transfer contract is explicitly empty",
			edit: func(d *element.Descriptor) {
				d.StateSchema = "schema://test/state/v1"
				d.StateTransfer = &element.StateTransferCapabilities{}
			},
			want: "state transfer declares no capabilities",
		},
		{
			name: "state transfer has no state schema",
			edit: func(d *element.Descriptor) {
				d.StateTransfer = &element.StateTransferCapabilities{Snapshot: true}
			},
			want: "state transfer requires a state schema",
		},
		{
			name: "state quiescence cannot transfer without snapshot",
			edit: func(d *element.Descriptor) {
				d.StateSchema = "schema://test/state/v1"
				d.StateTransfer = &element.StateTransferCapabilities{Quiesce: true}
			},
			want: "quiescence requires snapshot capability",
		},
		{
			name: "composite fingerprint is not a SHA-256",
			edit: func(d *element.Descriptor) { d.CompositeFingerprint = "sha256:short" },
			want: "invalid composite fingerprint",
		},
		{
			name: "composite fingerprint is not hexadecimal",
			edit: func(d *element.Descriptor) {
				d.CompositeFingerprint = "sha256:" + strings.Repeat("z", sha256.Size*2)
			},
			want: "invalid composite fingerprint",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			descriptor := baseDescriptor()
			test.edit(&descriptor)
			err := descriptor.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("descriptor error = %v, want one containing %q", err, test.want)
			}
		})
	}

	// A well-formed composite fingerprint is accepted.
	composite := baseDescriptor()
	composite.CompositeFingerprint = "sha256:" + strings.Repeat("a", sha256.Size*2)
	if err := composite.Validate(); err != nil {
		t.Fatalf("valid composite fingerprint = %v, want accepted", err)
	}

	// Snapshot-only predecessors and restore-only candidates are valid roles;
	// migration policy decides how two exact descriptor revisions may pair.
	for name, capabilities := range map[string]element.StateTransferCapabilities{
		"snapshot only": {Snapshot: true},
		"restore only":  {Restore: true},
		"quiesced snapshot and restore": {
			Snapshot: true, Restore: true, Quiesce: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			descriptor := baseDescriptor()
			descriptor.StateSchema = "schema://test/state/v1"
			descriptor.StateTransfer = &capabilities
			if err := descriptor.Validate(); err != nil {
				t.Fatalf("valid state-transfer capabilities = %v", err)
			}
		})
	}
}
