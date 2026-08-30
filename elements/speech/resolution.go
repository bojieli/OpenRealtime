package speech

import (
	"sort"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
)

const (
	ttsImplementationRevision      = "implementation:1"
	playbackImplementationRevision = "implementation:2"
)

const (
	ttsRuntimeID      = "builtin://openrealtime/elements/speech.TTS"
	ttsAdapterID      = "builtin://openrealtime/adapters/speech.TTS-api-v1"
	playbackRuntimeID = "builtin://openrealtime/elements/speech.Playback"
	playbackAdapterID = "builtin://openrealtime/adapters/speech.Playback-api-v1"
)

func reportTTSLiveResolution(
	reporter element.ResolutionReporter, descriptor v1.Descriptor,
) error {
	return reportSpeechProviderResolution(
		reporter, descriptor, "tts", "provider://openrealtime/api/v1/speech/tts/",
		ttsRuntimeID, ttsAdapterID, ttsImplementationRevision,
		"tts.synthesis", "openrealtime.api/v1.SpeechProvider",
	)
}

func reportPlaybackLiveResolution(
	reporter element.ResolutionReporter, descriptor v1.Descriptor,
) error {
	return reportSpeechProviderResolution(
		reporter, descriptor, "playback", "device://openrealtime/api/v1/speech/playback/",
		playbackRuntimeID, playbackAdapterID, playbackImplementationRevision,
		"playback.output", "openrealtime.action/SpeechSink-v1",
	)
}

func reportSpeechProviderResolution(
	reporter element.ResolutionReporter, descriptor v1.Descriptor, capabilityPrefix,
	providerPrefix, runtimeID, adapterID, implementationRevision, baseCapability, baseContract string,
) error {
	provider := liveidentity.Artifact{
		ID: providerPrefix + descriptor.Name, Revision: descriptor.Version,
	}
	adapter := liveidentity.Artifact{ID: adapterID, Revision: implementationRevision}
	capabilities := []element.CapabilityResolution{
		liveidentity.Capability(baseCapability, baseContract, provider, adapter),
	}
	selected := make([]string, 0, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		if enabled {
			selected = append(selected, string(capability))
		}
	}
	sort.Strings(selected)
	for _, capability := range selected {
		capabilities = append(capabilities, liveidentity.Capability(
			capabilityPrefix+"."+capability, "openrealtime.api/v1.Capability", provider, adapter,
		))
	}
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: runtimeID, Revision: implementationRevision,
	}, capabilities)
}
