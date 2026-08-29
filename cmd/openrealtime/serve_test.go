package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// A flag that parses and then does nothing is the failure mode this file
// exists for. Each of these asserts that a configured level reaches the policy
// set as a different policy, by name - a level that produced the same name as
// another level would be measuring nothing.

func defaultOptions() serveOptions {
	return serveOptions{
		rollout: "fast+slow", cadence: interaction.DefaultCadence,
		preparation: "endpoint-only", preparationPace: time.Second,
		bargeIn: "immediate", bargeInHold: 300 * time.Millisecond,
		observation: "endpoint-only", observers: "audio", policies: "none",
		transcriptPolicy: "none",
		narrator:         "session", fastProvider: "openai-compatible",
		visionTokenEnv: "OPENREALTIME_VISION_API_KEY",
	}
}

func TestEveryRolloutLevelIsADistinctPolicy(t *testing.T) {
	seen := make(map[string]string)
	for _, level := range []string{"fast-only", "fast+slow", "endpointed-slow-only"} {
		options := defaultOptions()
		options.rollout = level
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		name := policies.Rollout.Name()
		if previous, duplicate := seen[name]; duplicate {
			t.Fatalf("levels %q and %q are the same policy %q", previous, level, name)
		}
		seen[name] = level
	}
	options := defaultOptions()
	options.rollout = "telepathy"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown rollout must be refused rather than silently defaulted")
	}
}

func TestEveryBargeInLevelIsADistinctPolicy(t *testing.T) {
	seen := make(map[string]string)
	for _, level := range []string{"immediate", "sustained", "never"} {
		options := defaultOptions()
		options.bargeIn = level
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		name := policies.BargeIn.Name()
		if previous, duplicate := seen[name]; duplicate {
			t.Fatalf("levels %q and %q are the same policy %q", previous, level, name)
		}
		seen[name] = level
	}
	options := defaultOptions()
	options.bargeIn = "eventually"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown barge-in level must be refused")
	}
}

func TestSidecarCapabilitiesComposeIndependently(t *testing.T) {
	stack, err := parseStackCapabilities(
		"audio-input,audio-output,turn-generation,concurrent-io,native-floor," +
			"native-interaction,interaction-acts,transcription,text-injection",
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !stack.AudioInput || !stack.AudioOutput || !stack.TurnGeneration ||
		!stack.ConcurrentIO || !stack.NativeFloor || !stack.NativeInteraction ||
		!stack.InteractionActs || !stack.Transcription || !stack.TextInjection {
		t.Fatalf("a composed capability was lost: %+v", stack)
	}
	if _, err := parseStackCapabilities("audio-input,telepathy"); err == nil {
		t.Fatal("an unknown capability must be refused rather than ignored")
	}
}

func TestSidecarOwnershipAxesParseIndependently(t *testing.T) {
	interactionOwner, err := parseSidecarOwner("interaction", "model", "engine")
	if err != nil || interactionOwner != "model" {
		t.Fatalf("interaction owner: %q %v", interactionOwner, err)
	}
	floorOwner, err := parseSidecarOwner("floor", "engine", "model")
	if err != nil || floorOwner != "engine" {
		t.Fatalf("floor owner: %q %v", floorOwner, err)
	}
	if _, err := parseSidecarOwner("interaction", "duplex", "engine"); err == nil {
		t.Fatal("a species name is not an ownership declaration")
	}
}

// Preparation now really starts continuations, so the two levels have to be
// two things and the default has to be the one that spends nothing.
func TestPreparationDefaultsToOffAndIsSelectable(t *testing.T) {
	policies, err := buildPolicies(defaultOptions(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Preparation.Name() != "endpoint-only" {
		t.Fatalf("the default must speculate nothing, got %q", policies.Preparation.Name())
	}
	options := defaultOptions()
	options.preparation = "continuous"
	policies, err = buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(policies.Preparation.Name(), "continuous") {
		t.Fatalf("continuous preparation must be selectable, got %q", policies.Preparation.Name())
	}
	options.preparation = "sometimes"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown preparation level must be refused")
	}
}

// The trigger cadence is factor F4, so every level must reach the policy.
func TestTriggerCadenceReachesThePolicy(t *testing.T) {
	for _, cadence := range []time.Duration{
		50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond,
	} {
		options := defaultOptions()
		options.cadence = cadence
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", cadence, err)
		}
		if !strings.Contains(policies.Trigger.Name(), cadence.String()[:len(cadence.String())-2]) {
			t.Fatalf("cadence %s did not reach the trigger, which reports %q", cadence, policies.Trigger.Name())
		}
	}
}

// The stable-partial observation policy answers before the endpoint, so the
// deferral policy has to be willing to act before the endpoint. Composing the
// pair here is what keeps the flag usable; the binding refuses the incoherent
// one outright.
func TestStablePartialComposesADeferralThatCanActOnAPartial(t *testing.T) {
	options := defaultOptions()
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Deferral.Name() != "duplex" {
		t.Fatalf("endpoint-only observation keeps the shipped deferral, got %q", policies.Deferral.Name())
	}

	options.observation = "stable-partial"
	policies, err = buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(policies.Deferral.Name(), "allow-over-user") {
		t.Fatalf("stable-partial needs a deferral that admits a partial, got %q", policies.Deferral.Name())
	}

	options.observation = "eventually"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown observation policy must be refused")
	}
}

// Enabling a policy model without naming one is a configuration that cannot
// work, and it has to say so at startup rather than at the first decision.
func TestPolicyModelsNeedAModel(t *testing.T) {
	options := defaultOptions()
	options.policies = "backchannel"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("enabling a policy model with no model must be refused")
	}

	options.policyModel = "qwen-3b"
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Backchannel.Name() == "off" {
		t.Fatal("a configured backchannel policy model must replace the rule fallback")
	}

	options.policies = "everything"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown policy model name must be refused")
	}
}

func TestEventAwareTranscriptPolicyIsDistinctAndRequiresStreamingEvidence(t *testing.T) {
	options := defaultOptions()
	options.asrProvider = "deepgram"
	options.transcriptPolicy = "event-aware"
	options.transcriptPartialRules = "rules for partials"
	options.transcriptPartialActs = "listen,speak-through,interrupt"
	options.transcriptFinalRules = "rules for finals"
	options.transcriptFinalActs = "listen,answer"
	options.transcriptTimeout = 175 * time.Millisecond
	options.policyModel = "qwen-fast"
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build event-aware policy: %v", err)
	}
	if policies.TranscriptEvents == nil ||
		!strings.Contains(policies.Report().TranscriptEvents, "qwen-fast") {
		t.Fatalf("event-aware policy did not reach the policy set: %+v", policies.Report())
	}
	if policies.Interaction != nil {
		t.Fatal("the transcript policy silently enabled or mutated the existing interaction path")
	}

	options.asrProvider = "whisper-server"
	if _, err := buildPolicies(options, nil); err == nil || !strings.Contains(err.Error(), "streaming") {
		t.Fatalf("a batch recogniser was accepted for streaming event policy: %v", err)
	}
	options.asrProvider = "deepgram"
	options.transcriptPartialRules = ""
	if _, err := buildPolicies(options, nil); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("missing YAML-configurable partial rules were accepted: %v", err)
	}
}

func TestDeepgramLanguageDefaultsToEnglishAndRemainsConfigurable(t *testing.T) {
	options := defaultOptions()
	options.asrProvider = "deepgram"
	if got := recogniserLanguage(options); got != "en-US" {
		t.Fatalf("Deepgram default language = %q, want en-US", got)
	}
	options.asrLanguage = "multi"
	if got := recogniserLanguage(options); got != "multi" {
		t.Fatalf("configured ASR language = %q, want multi", got)
	}
	options.asrLanguage = ""
	options.language = "zh-CN"
	if got := recogniserLanguage(options); got != "zh-CN" {
		t.Fatalf("legacy shared language fallback = %q, want zh-CN", got)
	}
	options.asrProvider = "qwen-asr"
	options.language = ""
	if got := recogniserLanguage(options); got != "" {
		t.Fatalf("another recogniser inherited Deepgram's default: %q", got)
	}
}

// Turn projection reaches the conversation through the floor, so installing
// one without rebuilding the floor would leave it unused - the silent no-op a
// measured factor must never be.
func TestTurnProjectionRebuildsTheFloor(t *testing.T) {
	options := defaultOptions()
	options.policies = "turn-projection"
	options.policyModel = "qwen-3b"
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.TurnProjection.Name() == "vad-only" {
		t.Fatal("the projection policy was not installed")
	}
	if !strings.Contains(policies.Floor.Name(), policies.TurnProjection.Name()) {
		t.Fatalf("the floor must consult the projection it was given, floor=%q projection=%q",
			policies.Floor.Name(), policies.TurnProjection.Name())
	}
}

func TestInteractionFloorUsesExplicitEndpointSilence(t *testing.T) {
	options := defaultOptions()
	options.policies = "interaction"
	options.policyModel = "qwen-3b"
	options.interactionFloor = true
	options.endpointSilenceMS = 700
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	verdict := policies.Floor.Endpoint(interaction.Context{
		Revision:  interaction.Revision{ID: 1, StableText: "a complete request", SilenceNS: uint64(600 * time.Millisecond)},
		Situation: &interaction.Situation{Heard: "a complete request", Silence: "600ms"},
	})
	if verdict.Ended {
		t.Fatal("interaction floor ignored the explicit 700ms endpoint safety floor")
	}
}

// Factor F3's video-only level has to remove the recogniser, not just add a
// video observer alongside it.
func TestVideoOnlySelectsAnObserverSetWithoutAudio(t *testing.T) {
	for _, test := range []struct {
		configured string
		expected   []string
	}{
		{configured: "audio"},
		{configured: "audio+video"},
		{configured: "video", expected: []string{"video"}},
	} {
		selection, err := defaultObserverSet(test.configured)
		if err != nil {
			t.Fatalf("%s: %v", test.configured, err)
		}
		if strings.Join(selection, ",") != strings.Join(test.expected, ",") {
			t.Fatalf("%s selected %v, expected %v", test.configured, selection, test.expected)
		}
	}
	if _, err := defaultObserverSet("lidar"); err == nil {
		t.Fatal("an unknown observer set must be refused")
	}
}

// The governor is off unless a deployment states a capacity, because the unit
// is abstract and a made-up number would throttle a healthy deployment.
func TestTheGovernorIsOffUntilACapacityIsStated(t *testing.T) {
	governor, err := buildGovernor(defaultOptions())
	if err != nil || governor != nil {
		t.Fatalf("expected no governor, got %v err=%v", governor, err)
	}
	options := defaultOptions()
	options.gpuCapacity = 8
	governor, err = buildGovernor(options)
	if err != nil || governor == nil {
		t.Fatalf("expected a governor, got %v err=%v", governor, err)
	}
	snapshot := governor.Snapshot()
	if snapshot.Capacity != 8 || snapshot.ReservedInteractive <= 0 {
		t.Fatalf("the governor must reserve capacity for interactive work: %+v", snapshot)
	}
}

func TestServeRejectsFlagsItCannotHonour(t *testing.T) {
	var output strings.Builder
	for _, arguments := range [][]string{
		{"-binding", "telepathy"},
		{"-rollout", "sideways"},
		{"positional-argument"},
	} {
		if err := runServe(arguments, &output); err == nil {
			t.Fatalf("expected %v to be refused", arguments)
		}
	}
}

func TestFastComputerUseChangesAuthorityAndCannotBeASilentNoOp(t *testing.T) {
	options := defaultOptions()
	options.fastProvider = "vllm"
	options.fastURL = "http://127.0.0.1:8000/v1"
	options.fastModel = "vision-fast"
	options.explicit = map[string]bool{"fast-url": true, "fast-model": true}
	provider, err := buildFast(options)
	if err != nil {
		t.Fatalf("default fast provider: %v", err)
	}
	if provider.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		t.Fatal("fast must remain proposal-only by default")
	}
	options.fastComputerUse = true
	provider, err = buildFast(options)
	if err != nil {
		t.Fatalf("fast computer provider: %v", err)
	}
	if provider.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
		t.Fatal("the fast-computer flag did not reach the provider descriptor")
	}
	options.fastComputerUse = false
	options.fastBackgroundTools = true
	provider, err = buildFast(options)
	if err != nil {
		t.Fatalf("fast background provider: %v", err)
	}
	if provider.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
		t.Fatal("the fast-background flag did not reach the provider descriptor")
	}

	options.binding = "upstream"
	if _, _, err := buildBinding(options); err == nil || !strings.Contains(err.Error(), "cascade") {
		t.Fatalf("a cascade-only flag silently reached another binding: %v", err)
	}
}

func TestFastVoiceUsesDeterministicSampling(t *testing.T) {
	options := defaultOptions()
	options.fastProvider = "vllm"
	options.fastURL = "http://127.0.0.1:8000/v1"
	options.fastModel = "voice-model"
	options.explicit = map[string]bool{"fast-url": true, "fast-model": true}
	provider, err := buildFast(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.Descriptor().SamplingTemperature; got != "0" {
		t.Fatalf("fast sampling temperature = %q, want deterministic zero", got)
	}
}

// Factor F7's narrator composition was a label: both levels asked for
// -vision-model and both used whatever it named, so a report could say
// "session" about a separate model nobody in the session was using.
func TestNarratorCompositionNamesTheModelThatActuallyNarrates(t *testing.T) {
	options := defaultOptions()
	options.narrator = "session"
	options.fastURL = "http://127.0.0.1:8000/v1"
	options.fastModel = "qwen-vl"
	options.fastTokenEnv = "FAST_KEY"
	options.fastProvider = "openai-compatible"
	options.visionModel = "some-other-model"
	options.visionProvider = "openai-compatible"
	// The endpoint and model are only overrides when they were typed, so the
	// test has to say it typed them.
	options.explicit = map[string]bool{"fast-url": true, "fast-model": true, "fast-sees": true}

	// A session narrator is the session's own model, so it must not quietly
	// use the dedicated one that happens to be configured beside it.
	options.fastVision = true
	label, source, err := narratorComposition(options)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if label != "session" || source.model != "qwen-vl" ||
		source.url != options.fastURL || source.tokenEnv != "FAST_KEY" {
		t.Fatalf("a session narrator is the session's own model: %q %+v", label, source)
	}

	// And a model that cannot see is not narrating anything, so asking for it
	// is refused rather than silently producing nothing.
	options.fastVision = false
	if _, _, err := narratorComposition(options); err == nil {
		t.Fatal("a text-only fast model cannot be the session narrator")
	}

	// A fast provider on another dialect cannot narrate either, and the
	// refusal has to name the dialect rather than fail later on a frame.
	seeing := options
	seeing.fastVision = true
	seeing.fastProvider = "anthropic"
	if _, _, err := narratorComposition(seeing); err == nil {
		t.Fatal("a non-Chat-Completions fast provider cannot be the session narrator")
	}

	options.narrator = "dedicated"
	label, source, err = narratorComposition(options)
	if err != nil {
		t.Fatalf("dedicated: %v", err)
	}
	if label != "dedicated" || source.model != "some-other-model" ||
		source.tokenEnv != options.visionTokenEnv {
		t.Fatalf("a dedicated narrator is the one named: %q %+v", label, source)
	}

	options.visionModel = ""
	if _, _, err := narratorComposition(options); err == nil {
		t.Fatal("a dedicated narrator with no model must be refused")
	}
	options.narrator = "telepathy"
	if _, _, err := narratorComposition(options); err == nil {
		t.Fatal("an unknown narrator composition must be refused")
	}
}

// A flag default names one provider's credential variable. Reading it for
// whichever provider was actually selected would hand, say, a Gemini key to
// Anthropic and surface as an authentication failure at the vendor rather than
// a configuration error here.
func TestAnUntypedCredentialFlagDoesNotLeakOneProvidersKeyToAnother(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-secret")
	t.Setenv("OPENREALTIME_SLOW_API_KEY", "")
	options := defaultOptions()
	options.slowTokenEnv = "GEMINI_API_KEY"
	if got := roleCredential(options, "slow-token-env", options.slowTokenEnv, "OPENREALTIME_SLOW_API_KEY"); got != "" {
		t.Fatalf("an untyped flag must contribute nothing, got %q", got)
	}

	// Typing it is an instruction, and it is obeyed.
	options.explicit = map[string]bool{"slow-token-env": true}
	if got := roleCredential(options, "slow-token-env", options.slowTokenEnv, "OPENREALTIME_SLOW_API_KEY"); got != "gemini-secret" {
		t.Fatalf("a named variable must be read, got %q", got)
	}

	// The role's own variable is for running two profiles of one vendor.
	options.explicit = nil
	t.Setenv("OPENREALTIME_SLOW_API_KEY", "role-secret")
	if got := roleCredential(options, "slow-token-env", options.slowTokenEnv, "OPENREALTIME_SLOW_API_KEY"); got != "role-secret" {
		t.Fatalf("the role variable must be read, got %q", got)
	}
}

// The sight flags default to false. A false default that overrode the
// catalogue would withhold images from every multimodal provider unless the
// operator remembered a flag they had no reason to think they needed.
func TestAnUntypedSightFlagDefersToTheProvider(t *testing.T) {
	options := defaultOptions()
	if visionOverride(options, "fast-sees", false) != nil {
		t.Fatal("an untyped sight flag must defer to the provider catalogue")
	}
	options.explicit = map[string]bool{"fast-sees": true}
	declared := visionOverride(options, "fast-sees", false)
	if declared == nil || *declared {
		t.Fatalf("a typed sight flag must decide: %v", declared)
	}
}

// A recogniser advancing every 200ms must not inherit the budget of a model
// that may legitimately think for a minute. When a local recogniser wedged,
// every session against it went silent for as long as anyone would wait: no
// turn, no error, nothing in the log until teardown.
func TestTheRecogniserIsBoundedByItsOwnCadence(t *testing.T) {
	const shared = 2 * time.Minute
	if got := recogniserTimeout(200*time.Millisecond, shared); got >= shared {
		t.Fatalf("a 200ms cadence must not wait a reasoning model's timeout, got %s", got)
	}
	// A very short cadence must not produce a bound a healthy provider trips on.
	if got := recogniserTimeout(10*time.Millisecond, shared); got < 5*time.Second {
		t.Fatalf("the floor must survive a short cadence, got %s", got)
	}
	// A long cadence must not quietly exceed the deployment's own limit.
	if got := recogniserTimeout(time.Hour, shared); got != shared {
		t.Fatalf("the shared timeout is still the ceiling, got %s", got)
	}
}

func TestVoiceProfileIsAnIdentityTransform(t *testing.T) {
	options := defaultOptions()
	options.profile = "voice"
	options.fastProvider = "vllm"
	options.fastModel = "voice-model"
	options.fastTokens = 96
	options.slowProvider = "vllm"
	options.slowModel = "reasoner-model"
	options.slowEffort = "high"
	options.slowTokens = 2048
	options.requestTimeout = 2 * time.Minute
	options.explicit = map[string]bool{
		"fast-model": true, "slow-model": true,
	}
	before := options
	normalized, err := normalizeProfile(options)
	if err != nil {
		t.Fatalf("normalize voice: %v", err)
	}
	if !reflect.DeepEqual(normalized, before) {
		t.Fatalf("voice profile changed legacy configuration:\nbefore=%+v\nafter=%+v", before, normalized)
	}

	beforeFast, err := buildFast(before)
	if err != nil {
		t.Fatalf("build legacy fast: %v", err)
	}
	afterFast, err := buildFast(normalized)
	if err != nil {
		t.Fatalf("build profile fast: %v", err)
	}
	beforeSlow, err := buildSlow(before)
	if err != nil {
		t.Fatalf("build legacy slow: %v", err)
	}
	afterSlow, err := buildSlow(normalized)
	if err != nil {
		t.Fatalf("build profile slow: %v", err)
	}
	if !reflect.DeepEqual(beforeFast.Descriptor(), afterFast.Descriptor()) ||
		!reflect.DeepEqual(beforeSlow.Descriptor(), afterSlow.Descriptor()) {
		t.Fatalf("voice profile changed provider descriptors: before=%+v/%+v after=%+v/%+v",
			beforeFast.Descriptor(), beforeSlow.Descriptor(), afterFast.Descriptor(), afterSlow.Descriptor())
	}
	beforePolicies, err := buildPolicies(before, nil)
	if err != nil {
		t.Fatalf("build legacy policies: %v", err)
	}
	afterPolicies, err := buildPolicies(normalized, nil)
	if err != nil {
		t.Fatalf("build profile policies: %v", err)
	}
	if !reflect.DeepEqual(beforePolicies.Report(), afterPolicies.Report()) {
		t.Fatalf("voice profile changed policies: before=%+v after=%+v",
			beforePolicies.Report(), afterPolicies.Report())
	}
	if reflex, err := buildVisualReflex(normalized); err != nil || reflex != nil {
		t.Fatalf("voice profile instantiated a visual role: provider=%v err=%v", reflex, err)
	}
}

func TestVoiceVisionProfileSelectsCurrentFramesWithoutChangingVoiceRoles(t *testing.T) {
	options := defaultOptions()
	options.profile = "voice+vision"
	options.fastProvider = "vllm"
	options.fastModel = "voice-model"
	options.slowProvider = "vllm"
	options.slowModel = "reasoner-model"
	options.slowEffort = "high"
	options.reflexProvider = "vllm"
	options.reflexURL = "http://127.0.0.1:8004/v1"
	options.reflexModel = "qwen-vl-fast-local"
	options.reflexTokens = 48
	options.reflexTimeout = 650 * time.Millisecond
	options.explicit = map[string]bool{
		"profile": true, "fast-model": true, "slow-model": true,
		"visual-reflex-model": true, "visual-reflex-url": true,
	}
	voiceBefore, err := buildFast(options)
	if err != nil {
		t.Fatalf("voice before profile: %v", err)
	}
	slowBefore, err := buildSlow(options)
	if err != nil {
		t.Fatalf("slow before profile: %v", err)
	}
	normalized, err := normalizeProfile(options)
	if err != nil {
		t.Fatalf("normalize voice+vision: %v", err)
	}
	if normalized.observers != "audio+video" || normalized.components != "keyframe" {
		t.Fatalf("visual reflex profile did not select current frames: observers=%q components=%q",
			normalized.observers, normalized.components)
	}
	voiceAfter, _ := buildFast(normalized)
	slowAfter, _ := buildSlow(normalized)
	if !reflect.DeepEqual(voiceBefore.Descriptor(), voiceAfter.Descriptor()) ||
		!reflect.DeepEqual(slowBefore.Descriptor(), slowAfter.Descriptor()) {
		t.Fatal("enabling the visual role changed voice or slow cognition")
	}
	reflex, err := buildVisualReflex(normalized)
	if err != nil {
		t.Fatalf("build visual reflex: %v", err)
	}
	descriptor := reflex.Descriptor()
	if descriptor.Provider != "vllm" || descriptor.Model != "qwen-vl-fast-local" ||
		!descriptor.Vision || descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityExecute ||
		descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		t.Fatalf("unexpected independent visual role: %+v", descriptor)
	}
}

func TestVoiceVisionProfilePreservesExplicitObserverPolicy(t *testing.T) {
	options := defaultOptions()
	options.profile = "voice+vision"
	options.reflexModel = "vision"
	options.observers = "video"
	options.components = "keyframe+narration"
	options.explicit = map[string]bool{
		"profile": true, "visual-reflex-model": true,
		"observers": true, "observer-components": true,
	}
	normalized, err := normalizeProfile(options)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normalized.observers != "video" || normalized.components != "keyframe+narration" {
		t.Fatalf("profile overrode explicit interaction/perception policy: %+v", normalized)
	}
}

func TestVoiceVisionProfileAcceptsProtocolV3VisualSidecar(t *testing.T) {
	options := defaultOptions()
	options.binding = "omni+text-policy"
	options.profile = "voice+vision"
	options.sidecarCapabilities = "audio-input,audio-output,visual-input,turn-generation"
	options.sidecarProtocol = sidecar.VersionMultimodal
	options.explicit = map[string]bool{"profile": true, "binding": true}
	normalized, err := normalizeProfile(options)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.observers != "audio+video" {
		t.Fatalf("visual sidecar profile selected observers %q", normalized.observers)
	}
}

func TestLegacyVisualFlagsNormalizeIntoTheUnifiedProfile(t *testing.T) {
	options := defaultOptions()
	options.profile = "voice"
	options.observers = "audio+video"
	options.components = "keyframe+narration"
	options.explicit = map[string]bool{
		"observers": true, "observer-components": true,
	}
	normalized, err := normalizeProfile(options)
	if err != nil {
		t.Fatalf("normalize legacy visual flags: %v", err)
	}
	if normalized.profile != "voice+vision" || normalized.observers != options.observers ||
		normalized.components != options.components {
		t.Fatalf("legacy flags did not normalize without changing their choices: %+v", normalized)
	}
}

func TestVisualReflexCannotBeConfiguredAsANarrationOnlyNoOp(t *testing.T) {
	options := defaultOptions()
	options.profile = "voice+vision"
	options.reflexModel = "vision"
	options.components = "narration"
	options.explicit = map[string]bool{
		"profile": true, "visual-reflex-model": true, "observer-components": true,
	}
	if _, err := normalizeProfile(options); err == nil {
		t.Fatal("a reflex with no retained frame must be refused instead of silently never running")
	}
}
