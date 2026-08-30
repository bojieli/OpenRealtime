package perception

import (
	"sort"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
)

const (
	// ASR implementation:3 retains implementation:2 immutable operation
	// identity and additionally waits for an endpoint flush's exact final
	// admitted-audio item across independently scheduled input lanes.
	asrImplementationRevision    = "implementation:3"
	visualImplementationRevision = "implementation:1"
)

const (
	asrRuntimeID    = "builtin://openrealtime/elements/perception.ASR"
	asrAdapterID    = "builtin://openrealtime/adapters/perception.ASR-api-v1"
	visualRuntimeID = "builtin://openrealtime/elements/perception.VisualObserver"
	visualAdapterID = "builtin://openrealtime/adapters/perception.VisualObserver-narrator"
)

func reportASRLiveResolution(
	reporter element.ResolutionReporter, descriptor v1.Descriptor,
) error {
	provider := liveidentity.Artifact{
		ID:       "provider://openrealtime/api/v1/perception/" + descriptor.Name,
		Revision: descriptor.Version,
	}
	adapter := liveidentity.Artifact{ID: asrAdapterID, Revision: asrImplementationRevision}
	capabilities := []element.CapabilityResolution{liveidentity.Capability(
		"asr.transcription", "openrealtime.api/v1.PerceptionProvider", provider, adapter,
	)}
	selected := make([]string, 0, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		if enabled {
			selected = append(selected, string(capability))
		}
	}
	sort.Strings(selected)
	for _, capability := range selected {
		capabilities = append(capabilities, liveidentity.Capability(
			"asr."+capability, "openrealtime.api/v1.Capability", provider, adapter,
		))
	}
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: asrRuntimeID, Revision: asrImplementationRevision,
	}, capabilities)
}

func reportVisualLiveResolution(
	reporter element.ResolutionReporter, descriptor VisualProviderDescriptor,
) error {
	provider := liveidentity.Artifact{
		ID:       "provider://openrealtime/visual/" + descriptor.Name,
		Revision: descriptor.Revision, Digest: descriptor.Digest,
	}
	adapter := liveidentity.Artifact{ID: visualAdapterID, Revision: visualImplementationRevision}
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: visualRuntimeID, Revision: visualImplementationRevision,
	}, []element.CapabilityResolution{liveidentity.Capability(
		"vision.narration", "openrealtime.perception/Narrator-v1", provider, adapter,
	)})
}
