package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
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

func TestMeetingDeploymentsFromEnvironmentIsAllOrNothing(t *testing.T) {
	if deployments, present, err := meetingDeploymentsFromEnvironment(func(string) (string, bool) {
		return "", false
	}); err != nil || present || deployments != (meetingDeploymentIdentities{}) {
		t.Fatalf("empty deployment environment = %+v, present=%v, err=%v", deployments, present, err)
	}

	values := map[string]string{meetingModelDeploymentIDEnvironment: "model://partial"}
	lookup := func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
	if _, _, err := meetingDeploymentsFromEnvironment(lookup); err == nil ||
		!strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial deployment environment error = %v", err)
	}

	want := meetingProfileDeployments()
	for _, item := range []struct {
		identity inspect.ArtifactIdentity
		id       string
		revision string
		digest   string
	}{
		{want.Model, meetingModelDeploymentIDEnvironment, meetingModelDeploymentRevisionEnvironment, meetingModelDeploymentDigestEnvironment},
		{want.ASR, meetingASRDeploymentIDEnvironment, meetingASRDeploymentRevisionEnvironment, meetingASRDeploymentDigestEnvironment},
		{want.TTS, meetingTTSDeploymentIDEnvironment, meetingTTSDeploymentRevisionEnvironment, meetingTTSDeploymentDigestEnvironment},
		{want.Vision, meetingVisionDeploymentIDEnvironment, meetingVisionDeploymentRevisionEnvironment, meetingVisionDeploymentDigestEnvironment},
		{want.Background, meetingBackgroundDeploymentIDEnvironment, meetingBackgroundRevisionEnvironment, meetingBackgroundDigestEnvironment},
	} {
		values[item.id] = item.identity.ID
		values[item.revision] = item.identity.Revision
		values[item.digest] = item.identity.Digest
	}
	got, present, err := meetingDeploymentsFromEnvironment(lookup)
	if err != nil || !present || got != want {
		t.Fatalf("exact deployment environment = %+v, present=%v, err=%v", got, present, err)
	}
}

func TestMeetingRegistrationIsResourceFreeAndRetainsExactLazyReadiness(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENREALTIME_LOCAL_API_KEY", "local-secret-not-evidence")
	t.Setenv("OPENREALTIME_ASR_API_KEY", "asr-secret-not-evidence")
	t.Setenv("OPENREALTIME_TTS_API_KEY", "tts-secret-not-evidence")

	selected, err := newServeMeetingRegistration(
		meetingProfileExecutable(), meetingProfileDeployments(),
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
	if got := foreground.policies.Report().Rollout; got != "fast-only" {
		t.Fatalf("foreground rollout = %q", got)
	}
	if !foreground.inner.Capabilities().FastSlow {
		t.Fatal("cascade test precondition changed: inner binding no longer exposes its private slow slot")
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

func TestFreezeMeetingProfileBindsExactGraphResolutionAndDeployments(t *testing.T) {
	options := defaultMeetingProfileOptions()
	options.deployments = meetingProfileDeployments()
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
		frozen.Configuration.Background.Model != "gemini-3.7-flash" ||
		frozen.Configuration.Background.Deployment != options.deployments.Background {
		t.Fatalf("frozen Meeting profile = %+v", frozen)
	}
	if len(frozen.Resolution.Elements) != len(frozen.Plan.Graph().Nodes) ||
		frozen.Execution.Graph == nil ||
		frozen.Execution.Graph.Graph.Fingerprint != frozen.Plan.Graph().Fingerprint {
		t.Fatalf("frozen Meeting execution = %+v", frozen.Execution)
	}

	drifted := options
	drifted.deployments.Background.Digest = "sha256:" + strings.Repeat("6", 64)
	other, err := freezeProductionMeetingProfile(context.Background(), drifted, executable)
	if err != nil {
		t.Fatal(err)
	}
	if other.Profile.Fingerprint == frozen.Profile.Fingerprint ||
		other.Plan.Identity() == frozen.Plan.Identity() {
		t.Fatal("background deployment drift did not change exact Meeting profile and plan identities")
	}
}

func TestMeetingProfileRejectsCancellationAndInvalidDeploymentBeforeComposition(t *testing.T) {
	options := defaultMeetingProfileOptions()
	options.deployments = meetingProfileDeployments()
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

	options.deployments = meetingProfileDeployments()
	options.tokenEnv = ""
	if _, err := freezeProductionMeetingProfile(
		context.Background(), options, meetingProfileExecutable(),
	); err == nil || !strings.Contains(err.Error(), "server bounds") {
		t.Fatalf("unauthenticated Meeting profile error = %v", err)
	}
}
