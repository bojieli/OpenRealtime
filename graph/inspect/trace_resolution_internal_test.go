package inspect

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// validateTraceResolution is what makes a live trace's claim about *what code
// actually ran* trustworthy. Five of its refusals had no coverage, and they are
// the ones that separate a live observation from a plausible reconstruction.
func traceArtifact(id string) ArtifactIdentity {
	return ArtifactIdentity{ID: id, Revision: "1"}
}

func validTraceResolution() NodeResolution {
	return NodeResolution{
		Element: element.Identity{
			Name: "cognition.TextModel", Revision: 1,
			Digest: "sha256:" + strings.Repeat("b", 64),
		},
		Runtime:              traceArtifact("runtime/text-model"),
		RuntimeEvidence:      EvidenceLive,
		CapabilitiesEvidence: EvidenceLive,
		Capabilities: []CapabilityIdentity{
			{Name: "model.text", Contract: "Event<text.Delta>", Provider: traceArtifact("provider/vllm")},
		},
	}
}

func TestTraceResolutionOnlyAcceptsLiveEvidence(t *testing.T) {
	t.Parallel()
	limits := DefaultTraceLimits()
	if err := validateTraceResolution(validTraceResolution(), limits); err != nil {
		t.Fatalf("well-formed live resolution = %v, want accepted", err)
	}

	for _, test := range []struct {
		name string
		edit func(*NodeResolution)
		want string
	}{
		{
			// Registered evidence says "this is what the catalogue promised".
			// Only live evidence says "this is what answered". A trace that
			// accepted the former would report a promise as an observation.
			name: "capabilities are registered rather than live",
			edit: func(resolution *NodeResolution) {
				resolution.CapabilitiesEvidence = EvidenceRegistered
			},
			want: "capability evidence is",
		},
		{
			name: "capabilities carry no evidence at all",
			edit: func(resolution *NodeResolution) { resolution.CapabilitiesEvidence = "" },
			want: "capability evidence is",
		},
		{
			name: "runtime evidence is neither registered nor live",
			edit: func(resolution *NodeResolution) {
				resolution.RuntimeEvidence = ResolutionEvidence("declared")
			},
			want: "runtime evidence is",
		},
		{
			name: "capabilities are not in canonical order",
			edit: func(resolution *NodeResolution) {
				resolution.Capabilities = []CapabilityIdentity{
					{Name: "model.zeta", Provider: traceArtifact("provider/vllm")},
					{Name: "model.alpha", Provider: traceArtifact("provider/vllm")},
				}
			},
			want: "not canonical",
		},
		{
			name: "a capability repeats",
			edit: func(resolution *NodeResolution) {
				resolution.Capabilities = []CapabilityIdentity{
					{Name: "model.text", Provider: traceArtifact("provider/vllm")},
					{Name: "model.text", Provider: traceArtifact("provider/vllm")},
				}
			},
			want: "capabilit",
		},
		{
			// A capability whose provider cannot be named is a capability
			// nothing can be attributed to.
			name: "capability provider has no identity",
			edit: func(resolution *NodeResolution) {
				resolution.Capabilities = []CapabilityIdentity{
					{Name: "model.text", Provider: ArtifactIdentity{}},
				}
			},
			want: "provider: artifact identity",
		},
		{
			name: "capability adapter is present but unidentified",
			edit: func(resolution *NodeResolution) {
				empty := ArtifactIdentity{}
				resolution.Capabilities = []CapabilityIdentity{{
					Name: "model.text", Provider: traceArtifact("provider/vllm"), Adapter: &empty,
				}}
			},
			want: "adapter: artifact identity",
		},
		{
			name: "implementation selector is a placeholder rather than an identity",
			edit: func(resolution *NodeResolution) { resolution.Implementation = " builtin" },
			want: "exact canonical identity",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolution := validTraceResolution()
			test.edit(&resolution)
			err := validateTraceResolution(resolution, limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolution error = %v, want one containing %q", err, test.want)
			}
		})
	}

	t.Run("more capabilities than the trace limit", func(t *testing.T) {
		t.Parallel()
		resolution := validTraceResolution()
		bounded := limits
		bounded.MaxCapabilitiesPerNode = 1
		resolution.Capabilities = []CapabilityIdentity{
			{Name: "model.alpha", Provider: traceArtifact("provider/vllm")},
			{Name: "model.beta", Provider: traceArtifact("provider/vllm")},
		}
		err := validateTraceResolution(resolution, bounded)
		if err == nil || !strings.Contains(err.Error(), "limit is 1") {
			t.Fatalf("over-limit resolution = %v, want a capability limit refusal", err)
		}
	})
}
