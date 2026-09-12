package main

import (
	_ "embed"

	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
)

// These are the separate partial/final interaction policies used by the
// twelve-case Deepgram/Qwen/Gemini/Fish scenario pipeline. Keeping them in the
// executable makes room launches independent of a checkout or .runtime files.
//
//go:embed room/partial.txt
var roomPartialRules string

//go:embed room/final.txt
var roomFinalRules string

func defaultRoomProfileOptions() scenarioProfileOptions {
	selection := defaultScenarioProfileOptions()
	selection.name = "openrealtime.launch.conversation-room"
	selection.architecture = "cascade.composed-policy-direct-visual-speaker@1"
	selection.asrProvider = "deepgram"
	selection.asrModel = "nova-3"
	selection.asrURL = "wss://api.deepgram.com/v1/listen"
	selection.asrLanguage = "en-US,zh-CN"
	selection.asrKeyterms = []string{"sea bass"}
	selection.asrPartialMS = 0
	selection.asrEndpointingMS = 300
	selection.asrCadenceMS = 100
	selection.speakerURL = "http://127.0.0.1:8124/embed"
	selection.modelProvider = "google"
	selection.modelName = "gemini-3.7-flash"
	selection.modelURL = "https://generativelanguage.googleapis.com/v1beta"
	// Gemini shares its output ceiling with thinking; 128 can exhaust the
	// entire response before any audible answer is generated.
	selection.maxOutputTokens = 1024
	selection.continuationInstruction = "Every word you generate is spoken aloud. Answer the latest user request directly in the language they are using unless they requested translation. Runtime notes, observation labels, and playback annotations are context, never words to read aloud. " + selection.continuationInstruction
	// Repeated captured-context profiling favored 3.7/128 for low first-text
	// latency without the long-tail delays observed in the other Flash versions.
	selection.modelEffort = "128"
	selection.modelReason = "on"
	selection.transcriptPolicy = "event-aware"
	selection.transcriptTimeoutMS = 1000
	selection.transcriptPartialActs = "listen,speak-through,interrupt,act-silently,keep-speaking,stop-speaking"
	selection.transcriptFinalActs = "listen,answer,act-silently,keep-speaking,stop-speaking"
	selection.transcriptPartialRules = roomPartialRules
	selection.transcriptFinalRules = roomFinalRules
	// The word-timing role, not the speech recogniser. These name the
	// adapter's own constants rather than repeating an address, because the
	// room previously named the recogniser on :8003: it answers this route
	// with text and no word array, so every boundary fell back to the
	// proportional estimate and nothing said so.
	selection.wordTimingsURL = wordtimings.DefaultEndpoint
	selection.wordTimingsModel = wordtimings.DefaultModel
	selection.wordTimingsLanguage = "en"
	selection.wordTimingsIntervalMS = 900
	return selection
}

// defaultFilteredRoomProfileOptions is a separate opt-in pipeline. Noise is
// removed before EnergyAdmission can emit speech activity or feed Deepgram.
func defaultFilteredRoomProfileOptions() scenarioProfileOptions {
	selection := defaultRoomProfileOptions()
	selection.name = "openrealtime.launch.filtered-room"
	selection.noiseFilterURL = "http://127.0.0.1:8125"
	selection.noiseFilterTimeoutMS = 50
	return selection
}

// defaultTargetRoomProfileOptions enrolls the initial voice once, then extracts
// it from competing speech before Deepgram. No per-utterance identity service.
func defaultTargetRoomProfileOptions() scenarioProfileOptions {
	selection := defaultFilteredRoomProfileOptions()
	selection.name = "openrealtime.launch.target-room"
	selection.noiseFilterURL = "http://127.0.0.1:8126"
	selection.noiseFilterModel = "real-tse"
	selection.architecture = "cascade.composed-policy-direct-visual@1"
	selection.speakerURL = ""
	return selection
}
