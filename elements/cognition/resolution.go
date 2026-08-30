package cognition

import (
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
)

const cognitionImplementationRevision = "implementation:2"

const (
	textModelRuntimeID = "builtin://openrealtime/elements/cognition.TextModel"
	textModelAdapterID = "builtin://openrealtime/adapters/cognition.TextModel-continuation"
)

func reportTextModelLiveResolution(
	reporter element.ResolutionReporter, descriptor continuation.Descriptor,
) error {
	digest, err := descriptorDigest(descriptor)
	if err != nil {
		return err
	}
	provider := liveidentity.Artifact{
		ID: "model://" + descriptor.Provider, Revision: descriptor.Model, Digest: digest,
	}
	adapter := liveidentity.Artifact{
		ID: textModelAdapterID, Revision: cognitionImplementationRevision,
	}
	capabilities := []element.CapabilityResolution{
		liveidentity.Capability(
			"cognition.continuation", "openrealtime.continuation/Provider-v1",
			provider, adapter,
		),
		liveidentity.Capability(
			"cognition.reasoning", "openrealtime.continuation/Effort/"+string(descriptor.Effort),
			provider, adapter,
		),
	}
	if descriptor.Streaming {
		capabilities = append(capabilities, liveidentity.Capability(
			"cognition.streaming", "openrealtime.continuation/Emit-v1", provider, adapter,
		))
	}
	if descriptor.Vision {
		capabilities = append(capabilities, liveidentity.Capability(
			"cognition.vision", "openrealtime.continuation/Media-v1", provider, adapter,
		))
	}
	if descriptor.NativeStateType != "" {
		capabilities = append(capabilities, liveidentity.Capability(
			"cognition.native-state", descriptor.NativeStateType, provider, adapter,
		))
	}
	if descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityNone {
		capabilities = append(capabilities, liveidentity.Capability(
			"cognition.tool-calls", "openrealtime.continuation/ToolProposal-v1", provider, adapter,
		))
	}
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: textModelRuntimeID, Revision: cognitionImplementationRevision,
	}, capabilities)
}
