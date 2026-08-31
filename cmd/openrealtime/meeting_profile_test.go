package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/interaction"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
)

func meetingProfileArtifact(id, digestSymbol string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: id, Revision: "build-20260830",
		Digest: "sha256:" + strings.Repeat(digestSymbol, 64),
	}
}

func meetingProfileDeployments() meetingDeploymentIdentities {
	return meetingDeploymentIdentities{
		Model:  meetingProfileArtifact("model://openrealtime/meeting/qwen-fast", "1"),
		ASR:    meetingProfileArtifact("model://openrealtime/meeting/sensevoice", "2"),
		TTS:    meetingProfileArtifact("model://openrealtime/meeting/fish-speech", "3"),
		Vision: meetingProfileArtifact("model://openrealtime/meeting/qwen-vision", "4"),
		Background: meetingProfileArtifact(
			"model://google/gemini-3.7-flash", "5",
		),
	}
}

func meetingProfileExecutable() inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID:     "go://openrealtime/openrealtime-process",
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
}

type fixtureMeetingDeploymentVerifier struct {
	identities meetingDeploymentIdentities
	err        error
	resolve    atomic.Int32
	verify     atomic.Int32
}

func (verifier *fixtureMeetingDeploymentVerifier) Resolve(
	ctx context.Context,
) (meetingDeploymentIdentities, error) {
	verifier.resolve.Add(1)
	if err := context.Cause(ctx); err != nil {
		return meetingDeploymentIdentities{}, err
	}
	if verifier.err != nil {
		return meetingDeploymentIdentities{}, verifier.err
	}
	if err := verifier.identities.validate(); err != nil {
		return meetingDeploymentIdentities{}, err
	}
	return verifier.identities, nil
}

func (verifier *fixtureMeetingDeploymentVerifier) Verify(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	verifier.verify.Add(1)
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if verifier.err != nil {
		return verifier.err
	}
	if err := expected.validate(); err != nil {
		return err
	}
	if expected != verifier.identities {
		return errors.New("fixture Meeting deployment identity differs from live attestation")
	}
	return nil
}

func meetingProfileVerifier() *fixtureMeetingDeploymentVerifier {
	return &fixtureMeetingDeploymentVerifier{identities: meetingProfileDeployments()}
}

func TestMeetingRegistrationIsResourceFreeAndRetainsExactLazyReadiness(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENREALTIME_LOCAL_API_KEY", "local-secret-not-evidence")
	t.Setenv("OPENREALTIME_ASR_API_KEY", "asr-secret-not-evidence")
	t.Setenv("OPENREALTIME_TTS_API_KEY", "tts-secret-not-evidence")

	verifier := meetingProfileVerifier()
	selected, err := newServeMeetingRegistration(
		context.Background(), meetingProfileExecutable(), verifier.identities, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Application.Reference != meetinggraph.ApplicationReference ||
		selected.Adapter.Reference != meetinggraph.SessionAdapterReference ||
		selected.Configuration.Background.Model != meetingBackgroundModel {
		t.Fatalf("Meeting registration selection = %+v", selected)
	}
	application, err := graphs.MeetingAssistantApplicationConfig(selectedRegistration(selected), 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(application)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := selected.Application.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := graphlaunch.New(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.Graph().ID != meetinggraph.GraphID || len(prepared.Readiness) != 3 {
		t.Fatalf("prepared Meeting profile graph=%q readiness=%+v",
			prepared.Plan.Graph().ID, prepared.Readiness)
	}
	if got := prepared.Binding.Capabilities().Observers; !slices.Equal(got, []string{"audio", "screen"}) {
		t.Fatalf("Meeting binding observers = %v", got)
	}
	if got := prepared.Binding.Capabilities(); !got.Stack.VisualInput || !got.Stack.TextInjection || !got.FastSlow {
		t.Fatalf("Meeting foreground capabilities = %+v", got)
	}
	if err := prepared.Readiness[0].Check(context.Background()); err != nil {
		t.Fatalf("foreground readiness = %v", err)
	}
	if err := prepared.Readiness[1].Check(context.Background()); err != nil {
		t.Fatalf("visual readiness = %v", err)
	}
	if err := prepared.Readiness[2].Check(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "credential") {
		t.Fatalf("background readiness without Gemini credential = %v", err)
	}
	t.Setenv("GEMINI_API_KEY", "gemini-secret-not-evidence")
	if err := prepared.Readiness[2].Check(context.Background()); err != nil {
		t.Fatalf("background readiness with credential = %v", err)
	}

	retained, err := json.Marshal(struct {
		Configuration meetingLocalConfiguration `json:"configuration"`
		Profile       launchprofile.Document    `json:"profile"`
	}{Configuration: selected.Configuration})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"local-secret-not-evidence", "asr-secret-not-evidence",
		"tts-secret-not-evidence", "gemini-secret-not-evidence",
	} {
		if bytes.Contains(retained, []byte(secret)) {
			t.Fatalf("retained Meeting metadata contains credential %q", secret)
		}
	}
}

func TestMeetingForegroundCompositionDisablesPrivateSlowLane(t *testing.T) {
	config := defaultMeetingLocalConfiguration(
		meetingProfileExecutable(), meetingProfileDeployments(),
	).Foreground
	binding, err := newMeetingForegroundBinding(context.Background(), config, legacy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	foreground, ok := binding.(*meetingForegroundBinding)
	if !ok {
		t.Fatalf("foreground binding type = %T", binding)
	}
	if binding.Name() != meetingForegroundBindingName ||
		binding.Ownership() != meetingForegroundOwnership() {
		t.Fatalf("foreground identity name=%q ownership=%+v", binding.Name(), binding.Ownership())
	}
	capabilities := binding.Capabilities()
	if capabilities.FastSlow || !capabilities.Video || !capabilities.Observations ||
		!capabilities.ManualTurns ||
		!slices.Equal(capabilities.Observers, []string{"audio", "screen"}) {
		t.Fatalf("foreground capabilities = %+v", capabilities)
	}
	if got := foreground.policies.Report().Rollout; got != "meeting-fast-tool-continuations" {
		t.Fatalf("foreground rollout = %q", got)
	}
	if descriptor := meetingForegroundLLMRequest(config).ToolAuthority; descriptor != continuation.ToolAuthorityExecute {
		t.Fatalf("foreground tool authority = %q, want execute", descriptor)
	}
	reflex := meetingVisualReflexLLMRequest(config)
	if reflex.Provider != config.ModelProvider || reflex.Model != config.Model ||
		reflex.BaseURL != config.ModelURL || reflex.Phase != meetingForegroundLLMRequest(config).Phase ||
		reflex.ToolAuthority != continuation.ToolAuthorityExecute ||
		reflex.SpeechAuthority != continuation.SpeechAuthoritySilent ||
		reflex.Vision == nil || !*reflex.Vision ||
		reflex.RequestTimeout != 2_000_000_000 {
		t.Fatalf("Meeting visual reflex request = %+v", reflex)
	}
	if !foreground.inner.Capabilities().FastSlow {
		t.Fatal("cascade test precondition changed: inner binding no longer exposes its private slow slot")
	}
	runtime, err := binding.Start(context.Background(), legacy.Options{Sink: meetingProfileProbeSink{}})
	if err != nil {
		t.Fatal(err)
	}
	status := runtime.Status()
	if status.Fast != meetingLocalModelProvider+"/"+meetingLocalModelName ||
		status.Reflex != meetingLocalModelProvider+"/"+meetingLocalModelName ||
		status.Slow != "" || status.Tools.Fast != string(continuation.ToolAuthorityExecute) {
		t.Fatalf("Meeting foreground live role status = %+v", status)
	}
	if err := runtime.Close(context.Background(), errors.New("composition test complete")); err != nil {
		t.Fatal(err)
	}
	dormant := meetingDormantProvider{descriptor: meetingDormantDescriptor()}
	if dormant.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityExecute ||
		dormant.Descriptor().EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		t.Fatalf("dormant foreground descriptor = %+v", dormant.Descriptor())
	}
	if _, err := dormant.Continue(context.Background(), continuation.Request{}, nil); err == nil ||
		!strings.Contains(err.Error(), "graph owns background") {
		t.Fatalf("dormant foreground slow provider error = %v", err)
	}
}

func TestMeetingForegroundRolloutContinuesToolResultsWithoutPrivateSlowWork(t *testing.T) {
	policy := meetingForegroundRollout{}
	for _, test := range []struct {
		name  string
		cause interaction.Cause
	}{
		{name: "observation", cause: interaction.Cause{Observation: true}},
		{name: "tool result", cause: interaction.Cause{ToolResult: true}},
		{name: "tool error", cause: interaction.Cause{ToolError: true}},
		{name: "composite resume", cause: interaction.Cause{CompositeResume: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			steps := policy.Plan(interaction.RolloutInput{Cause: test.cause})
			if len(steps) != 1 || steps[0].Kind != interaction.StepFast {
				t.Fatalf("Meeting foreground steps = %+v", steps)
			}
		})
	}
	for _, cause := range []interaction.Cause{{Escalated: true}, {BackgroundResult: true}} {
		if steps := policy.Plan(interaction.RolloutInput{Cause: cause}); len(steps) != 0 {
			t.Fatalf("Meeting foreground scheduled private work for %+v: %+v", cause, steps)
		}
	}
}

func TestMeetingForegroundInstructionKeepsActionsSilentCompleteAndNonRepeating(t *testing.T) {
	for _, required := range []string{
		"successful tool result is not complete",
		"continue until the user's requested sequence is complete",
		"use its declared visible control silently",
		"screen observation or background result alone does not authorize a spoken response",
		"never repeat an answer already given",
	} {
		if !strings.Contains(meetingForegroundInstruction, required) {
			t.Fatalf("Meeting foreground instruction omitted %q: %q", required, meetingForegroundInstruction)
		}
	}
	for _, benchmarkSpecific := range []string{
		"launch review", "share screen", "deployment alert", "18.4",
	} {
		if strings.Contains(strings.ToLower(meetingForegroundInstruction), benchmarkSpecific) {
			t.Fatalf("Meeting foreground instruction embeds benchmark fixture %q", benchmarkSpecific)
		}
	}
}

func TestDefaultMeetingInspectionLeaseCoversFullCandidateCaseAndAttestation(t *testing.T) {
	const minimumLeaseMS = 120_000
	if got := defaultMeetingProfileOptions().inspectionTTL; got < minimumLeaseMS {
		t.Fatalf("default Meeting inspection lease = %d ms, want at least %d", got, minimumLeaseMS)
	}
}

func TestFreezeMeetingProfileBindsExactGraphResolutionAndDeployments(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "meeting-profile-fixture-credential")
	t.Setenv("GOOGLE_API_KEY", "")
	options := defaultMeetingProfileOptions()
	verifier := meetingProfileVerifier()
	options.deployments = verifier.identities
	options.verifier = verifier
	executable := meetingProfileExecutable()
	frozen, err := freezeProductionMeetingProfile(context.Background(), options, executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := frozen.Profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := frozen.Execution.Validate(); err != nil {
		t.Fatal(err)
	}
	if frozen.Plan == nil || frozen.Plan.Graph().ID != meetinggraph.GraphID ||
		frozen.Profile.Plan != frozen.Plan.Identity() ||
		frozen.Profile.Server.Model != meetingLocalModelName ||
		frozen.Profile.Server.TranscriptionModel != meetingLocalASRModel ||
		frozen.Profile.Server.TokenEnvironment != "OPENREALTIME_TOKEN" ||
		frozen.Profile.Server.VideoLimits.FPSCap != 5 ||
		frozen.Profile.Server.VideoLimits.FPSCap !=
			(frozen.Configuration.Foreground.FrameRateMilliHz+999)/1_000 ||
		frozen.Configuration.FormatVersion != 2 ||
		frozen.Configuration.Foreground.TTSVoice != "default" ||
		frozen.Configuration.Foreground.VisualReflexMaxTokens != 96 ||
		frozen.Configuration.Foreground.VisualReflexTimeoutMS != 2_000 ||
		!frozen.Configuration.Foreground.AttachKeyframes ||
		frozen.Configuration.Background.Model != "gemini-3.7-flash" ||
		frozen.Configuration.Background.Deployment != options.deployments.Background {
		t.Fatalf("frozen Meeting profile = %+v", frozen)
	}
	if len(frozen.Resolution.Elements) != len(frozen.Plan.Graph().Nodes) ||
		frozen.Resolution.Deployment == nil ||
		frozen.Resolution.Deployment.PrivateDeploymentFingerprint == "" ||
		frozen.Execution.Graph == nil ||
		frozen.Execution.Graph.Graph.Fingerprint != frozen.Plan.Graph().Fingerprint ||
		frozen.Execution.Graph.Deployment == nil ||
		frozen.Execution.Graph.Deployment.PrivateDeploymentFingerprint !=
			frozen.Resolution.Deployment.PrivateDeploymentFingerprint {
		t.Fatalf("frozen Meeting execution = %+v", frozen.Execution)
	}

	drifted := options
	drifted.deployments.Background.Digest = "sha256:" + strings.Repeat("6", 64)
	drifted.verifier = &fixtureMeetingDeploymentVerifier{identities: drifted.deployments}
	other, err := freezeProductionMeetingProfile(context.Background(), drifted, executable)
	if err != nil {
		t.Fatal(err)
	}
	if other.Profile.Fingerprint == frozen.Profile.Fingerprint ||
		other.Plan.Identity() == frozen.Plan.Identity() {
		t.Fatal("background deployment drift did not change exact Meeting profile and plan identities")
	}

	reflexDrift := options
	reflexDrift.verifier = verifier
	reflexFrozen, err := freezeProductionMeetingProfile(
		context.Background(), reflexDrift, executable,
	)
	if err != nil {
		t.Fatal(err)
	}
	reflexFrozen.Configuration.Foreground.VisualReflexTimeoutMS++
	reflexArtifact, err := meetingConfigurationArtifact(
		meetingRuntimeArtifactID, reflexFrozen.Configuration.Foreground, executable,
	)
	if err != nil {
		t.Fatal(err)
	}
	originalArtifact, err := meetingConfigurationArtifact(
		meetingRuntimeArtifactID, frozen.Configuration.Foreground, executable,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reflexArtifact == originalArtifact {
		t.Fatal("visual reflex bound drift did not change Meeting foreground artifact")
	}
}

func TestMeetingProfileRejectsCancellationAndInvalidDeploymentBeforeComposition(t *testing.T) {
	options := defaultMeetingProfileOptions()
	verifier := meetingProfileVerifier()
	options.deployments = verifier.identities
	options.verifier = verifier
	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("cancel Meeting profile freeze")
	cancel(want)
	if _, err := freezeProductionMeetingProfile(ctx, options, meetingProfileExecutable()); !errors.Is(err, want) {
		t.Fatalf("cancelled Meeting freeze error = %v", err)
	}

	options.deployments.Vision.Digest = "sha256:short"
	if _, err := freezeProductionMeetingProfile(
		context.Background(), options, meetingProfileExecutable(),
	); err == nil || !strings.Contains(err.Error(), "vision deployment") {
		t.Fatalf("invalid Meeting deployment error = %v", err)
	}

	options.deployments = verifier.identities
	options.tokenEnv = ""
	if _, err := freezeProductionMeetingProfile(
		context.Background(), options, meetingProfileExecutable(),
	); err == nil || !strings.Contains(err.Error(), "server bounds") {
		t.Fatalf("unauthenticated Meeting profile error = %v", err)
	}

	options = defaultMeetingProfileOptions()
	options.deployments = verifier.identities
	if _, err := freezeProductionMeetingProfile(
		context.Background(), options, meetingProfileExecutable(),
	); err == nil || !strings.Contains(err.Error(), "without a deployment verifier") {
		t.Fatalf("unverified Meeting profile error = %v", err)
	}
}

func TestMeetingForegroundRejectsUnboundedVisualReflexBeforeProviderComposition(t *testing.T) {
	config := defaultMeetingLocalConfiguration(
		meetingProfileExecutable(), meetingProfileDeployments(),
	).Foreground
	for _, mutate := range []func(*meetingLocalForegroundConfig){
		func(config *meetingLocalForegroundConfig) { config.VisualReflexMaxTokens = 0 },
		func(config *meetingLocalForegroundConfig) { config.VisualReflexTimeoutMS = 0 },
	} {
		candidate := config
		mutate(&candidate)
		if _, err := newMeetingForegroundBinding(
			context.Background(), candidate, legacy.Options{},
		); err == nil || !strings.Contains(err.Error(), "positive token and timeout bounds") {
			t.Fatalf("unbounded visual reflex error = %v", err)
		}
	}
}
