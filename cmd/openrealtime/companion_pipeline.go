package main

import _ "embed"

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
	selection.wordTimingsURL = "http://127.0.0.1:8003/v1/audio/transcriptions"
	selection.wordTimingsModel = "whisper-turbo"
	selection.wordTimingsLanguage = "en"
	selection.wordTimingsIntervalMS = 900
	return selection
}
