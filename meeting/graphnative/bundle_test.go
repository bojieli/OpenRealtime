package graphnative_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/gateway"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

func TestMeetingBundleSealsExactProviderWithoutAcquiringPlugins(t *testing.T) {
	fixture := newProviderFixture(t)
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.Graph().ID != meetinggraph.GraphID ||
		result.Evidence.Graph != meetinggraph.GraphID || result.Evidence.Profiles == nil ||
		result.CatalogEntry.GraphID != meetinggraph.GraphID || result.CatalogEntry.Plan != result.Plan.Identity() ||
		result.Binding.Graph().Fingerprint != result.Plan.Graph().Fingerprint {
		t.Fatalf("meeting provider graph = %+v", result.Binding.Graph())
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatalf("provider construction acquired mount=%d adapter=%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load())
	}
	capabilities := result.Binding.SessionAdapterProfile().Capabilities
	if !capabilities.Video || !capabilities.ComputerUse || !capabilities.Observations ||
		!capabilities.FastSlow || !capabilities.ManualTurns ||
		len(capabilities.Observers) != 1 || capabilities.Observers[0] != "screen" {
		t.Fatalf("meeting adapter capabilities = %+v", capabilities)
	}
	if result.Binding.Name() != "openrealtime.meeting.cascade" {
		t.Fatalf("meeting binding name = %q", result.Binding.Name())
	}
}

func TestMeetingLaunchConfigIsPureAndExposesExactSelection(t *testing.T) {
	fixture := newProviderFixture(t)
	launch, err := meetinggraph.LaunchConfig(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("resource-free launch configuration invoked a plugin factory")
	}
	want := fixture.config.Adapter
	if launch.Adapter.Reference != want.Reference ||
		launch.Adapter.RuntimeArtifact != want.Artifact ||
		launch.Adapter.ProfileName != want.Profile.Name ||
		launch.Adapter.ProfileRevision != want.Profile.Revision {
		t.Fatalf("meeting adapter selection = %+v", launch.Adapter)
	}
	if launch.PlanOptions.Catalog == nil || launch.PlanOptions.SchemaResolver == nil ||
		launch.PlanOptions.Loader == nil {
		t.Fatal("meeting launch configuration omitted compiler contracts")
	}
	if _, err := graphlaunch.New(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("generic launcher acquired a session resource")
	}
}

func TestMeetingProviderFailsClosedBeforeAdapterWhenDependencyServiceDrifts(t *testing.T) {
	fixture := newProviderFixture(t)
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Binding.Start(context.Background(), legacy.Options{
		SessionID: "meeting-fail-closed", Sink: meetingSink{},
	})
	if err == nil || !strings.Contains(err.Error(), "registry service has type struct {}") {
		t.Fatalf("Start() error = %v", err)
	}
	if fixture.mountFactories.Load() != 5 {
		t.Fatalf("mount dependency factory calls = %d, want 5", fixture.mountFactories.Load())
	}
	if fixture.adapterFactories.Load() != 0 {
		t.Fatalf("adapter factory ran after dependency drift: %d", fixture.adapterFactories.Load())
	}
}

func TestMeetingProviderMountsExactGraphAndCleansUpWhenAdapterRefuses(t *testing.T) {
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	_ = installValidMountServices(
		t, &fixture, &modelDials, &visualFactories, &visualCloses, nil, fixtureVisualConfig{},
	)
	want := errors.New("fixture adapter refusal")
	fixture.config.Adapter.Factory = func(
		_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		if mounted == nil || mounted.Graph().ID != meetinggraph.GraphID ||
			profile.GraphFingerprint != mounted.Graph().Fingerprint {
			return nil, errors.New("adapter received a drifted mounted graph")
		}
		return nil, want
	}
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := result.Binding.Start(context.Background(), legacy.Options{
		SessionID: "meeting-exact-mount", Sink: meetingSink{},
	})
	if runtime != nil || !errors.Is(err, want) {
		t.Fatalf("Start() = (%T, %v), want adapter refusal", runtime, err)
	}
	if fixture.mountFactories.Load() != 5 || fixture.adapterFactories.Load() != 1 {
		t.Fatalf("factory calls mount=%d adapter=%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load())
	}
	if modelDials.Load() != 0 {
		t.Fatalf("model dialed before graph Run: %d", modelDials.Load())
	}
	if visualFactories.Load() != 1 || visualCloses.Load() != 1 {
		t.Fatalf("visual lifecycle create=%d close=%d",
			visualFactories.Load(), visualCloses.Load())
	}
}

func TestMeetingProviderRunsAndReportsLiveGraphWithoutCredentials(t *testing.T) {
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	created := make(chan *fixtureExternalSession, 1)
	backgroundRequests := installValidMountServices(t, &fixture, &modelDials, &visualFactories, &visualCloses,
		func(_ context.Context, hello sidecar.Message) (modelelements.Session, error) {
			session := newFixtureExternalSession(hello)
			created <- session
			return session, nil
		}, fixtureVisualConfig{})
	fixture.config.Adapter.Factory = func(
		_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		if mounted == nil || profile.GraphFingerprint != mounted.Graph().Fingerprint {
			return nil, errors.New("adapter received a drifted mounted graph")
		}
		return newFixtureSessionAdapter(mounted, options)
	}
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := result.Binding.Start(context.Background(), legacy.Options{
		SessionID: "meeting-live-fixture", Sink: meetingSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*graphbinding.NativeRuntime)
	if !ok {
		t.Fatalf("meeting runtime has type %T", runtime)
	}
	var external *fixtureExternalSession
	select {
	case external = <-created:
	case <-time.After(2 * time.Second):
		_ = runtime.Close(context.Background(), errors.New("test timeout"))
		t.Fatal("foreground deployment was not dialed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		live := native.Live()
		ready := true
		for _, nodeID := range []string{
			"screen_fork", "foreground", "screen_observer", "background_model", "background_injection",
		} {
			node, found := live.Nodes[nodeID]
			if !found || node.Resolution == nil || node.Resolution.RuntimeEvidence != inspect.EvidenceLive {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		select {
		case <-native.Done():
			t.Fatalf("meeting graph stopped during readiness: %+v", native.Live())
		default:
		}
		if time.Now().After(deadline) {
			_ = runtime.Close(context.Background(), errors.New("test timeout"))
			t.Fatalf("meeting graph did not report live identities: %+v", live.Nodes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	status := runtime.Status()
	if status.Graph.ID != meetinggraph.GraphID ||
		status.Graph.Fingerprint != result.Plan.Graph().Fingerprint || status.Profile == "" {
		t.Fatalf("meeting runtime status = %+v", status)
	}
	observation := perception.Observation{
		Text: "Actually, use overview.", Observer: "foreground", Source: "microphone",
		Authority: trajectory.AuthorityUser, Revision: 1, Final: true, OccurredNS: 100,
	}
	encodedObservation, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	external.frames <- sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "transcript",
		Envelope: &sidecar.WireEnvelope{
			Type: modelelements.TranscriptType(), ItemID: "correction-1",
			SessionID: "meeting-live-fixture", SourceID: "microphone",
			RunID: "foreground-1", Sequence: 1, CaptureNS: 100,
			JSON: encodedObservation,
		},
	}
	select {
	case request := <-backgroundRequests:
		if request.Invocation.Instruction == "" || request.Trajectory.Version != 1 {
			t.Fatalf("background request = %+v", request)
		}
	case <-time.After(2 * time.Second):
		live := native.Live()
		for _, edge := range result.Plan.Graph().Edges {
			if state := live.Edges[edge.ID]; state.Enqueued != 0 || state.Backpressure != 0 {
				t.Logf("edge %s %s -> %s: %+v", edge.ID, edge.From.String(), edge.To.String(), state)
			}
		}
		_ = runtime.Close(context.Background(), errors.New("test timeout"))
		t.Fatal("background continuation was not invoked")
	}
	var injectionFrame sidecar.Message
	responseDeadline := time.After(2 * time.Second)
	for injectionFrame.Envelope == nil {
		select {
		case message := <-external.sent:
			switch message.Port {
			case "text":
				injectionFrame = message
			case "trigger":
				t.Fatalf("background completion autonomously reached foreground trigger: %+v", message)
			}
		case <-native.Done():
			t.Fatalf("meeting graph stopped during background handoff: %+v", native.Live())
		case <-responseDeadline:
			_ = runtime.Close(context.Background(), errors.New("test timeout"))
			t.Fatalf("background handoff missing retained context: live=%+v", native.Live())
		}
	}
	var injection meetinggraph.ContextInjection
	if err := json.Unmarshal(injectionFrame.Envelope.JSON, &injection); err != nil {
		t.Fatal(err)
	}
	if injection.Role != "background" || injection.Source != "meeting.background" ||
		injection.Text != "grounded background" || injection.RunID == "" {
		t.Fatalf("background context injection = %+v", injection)
	}
	awaitMeetingEdgeDequeued(t, native, "background_injection.trigger", "background_trigger_drop.in")
	awaitMeetingEdgeDequeued(t, native, "background_injection.outcome", "background_outcome_copy.in")
	select {
	case message := <-external.sent:
		if message.Port == "trigger" {
			t.Fatalf("completed background work opened a foreground generation: %+v", message)
		}
	default:
	}
	if err := runtime.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if fixture.mountFactories.Load() != 5 || fixture.adapterFactories.Load() != 1 ||
		modelDials.Load() != 1 || external.closes.Load() != 1 ||
		visualFactories.Load() != 1 || visualCloses.Load() != 1 {
		t.Fatalf("live lifecycle mount=%d adapter=%d dial=%d external-close=%d visual=%d/%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load(), modelDials.Load(),
			external.closes.Load(), visualFactories.Load(), visualCloses.Load())
	}
}

func TestMountedMeetingForegroundQuarantinePreservesOrderedSafeSpeech(t *testing.T) {
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	created := make(chan *fixtureExternalSession, 1)
	ttsPlans := make(chan v1.SpeechPlan, 1)
	ttsRelease := make(chan struct{})
	graphPCM := []byte{9, 0, 8, 0, 7, 0}
	_ = installValidMountServices(t, &fixture, &modelDials, &visualFactories, &visualCloses,
		func(_ context.Context, hello sidecar.Message) (modelelements.Session, error) {
			session := newFixtureExternalSession(hello)
			created <- session
			return session, nil
		}, fixtureVisualConfig{ttsPlans: ttsPlans, ttsPCM: graphPCM, ttsRelease: ttsRelease})
	realAdapter := meetinggraph.SessionAdapterFactory(meetinggraph.SessionAdapterConfig{})
	fixture.config.Adapter.Factory = func(
		ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		return realAdapter(ctx, mounted, options, profile)
	}
	provider, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	sink := &quarantineMeetingSink{done: make(chan struct{})}
	const sessionID = "meeting-quarantine-mounted"
	runtime, err := provider.Binding.Start(context.Background(), legacy.Options{
		SessionID: sessionID, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*graphbinding.NativeRuntime)
	if !ok {
		t.Fatalf("mounted Meeting runtime has type %T", runtime)
	}
	defer func() {
		if closeErr := runtime.Close(context.Background(), errors.New("test complete")); closeErr != nil {
			t.Errorf("close mounted Meeting runtime: %v", closeErr)
		}
	}()

	var external *fixtureExternalSession
	select {
	case external = <-created:
		external.leaveFramesOpen = true
	case <-time.After(2 * time.Second):
		t.Fatal("mounted Meeting foreground deployment was not dialed")
	}
	const runID = "foreground-quarantine-run"
	utterance := action.Utterance{ID: "foreground-quarantine-speech"}
	frames := []struct {
		port     string
		sequence uint64
		payload  any
	}{
		{"audio_out", 1, speechelements.AudioFrame{
			Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
		}},
		{"text_out", 2, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin, Index: 0,
		}},
		{"text_out", 3, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 1, Text: "Normal.\n",
		}},
		{"audio_out", 4, speechelements.AudioFrame{
			Kind: speechelements.AudioChunk, UtteranceID: utterance.ID,
			Chunk: v1.SpeechChunk{
				ChunkID: "quarantine-audio-1", CandidateID: utterance.ID,
				SampleRateHz: 24_000, PCM16LE: []byte{1, 0, 2, 0},
			},
		}},
		{"text_out", 5, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 2, Text: toolCallOpenForMeetingTest,
		}},
		{"text_out", 6, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 3,
			Text: `{"name":"hidden","arguments":{"secret":"never-speak"}}</tool_call>`,
		}},
		{"text_out", 7, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 4, Text: "Terminal ordinary <tool",
		}},
		{"audio_out", 8, speechelements.AudioFrame{
			Kind: speechelements.AudioChunk, UtteranceID: utterance.ID,
			Chunk: v1.SpeechChunk{
				ChunkID: "quarantine-native-audio-2", CandidateID: utterance.ID,
				SampleOffset: 2, SampleRateHz: 24_000, PCM16LE: []byte{3, 0, 4, 0}, Final: true,
			},
		}},
	}
	for index, frame := range frames {
		pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
			fmt.Sprintf("foreground-response-%d", index+1), frame.port, frame.sequence, frame.payload)
	}

	rawAssistant := "Normal.\n" + toolCallOpenForMeetingTest +
		`{"name":"hidden","arguments":{"secret":"never-speak"}}</tool_call>` +
		"Terminal ordinary <tool"
	result := cognitionelements.Result{
		RunID: runID, ProviderReference: meetinggraph.ForegroundDeploymentReference,
		Descriptor: continuation.Descriptor{
			Provider: "meeting-fixture", Model: "foreground", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
			ToolAuthority:   continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
			NativeStateType: "meeting-fixture/native.v1",
		},
		Invocation: continuation.Invocation{Instruction: "answer"},
		Outputs: []cognitionelements.PreparedOutput{{
			Kind: cognitionelements.PreparedAssistant, Text: rawAssistant,
		}},
		AssistantText: rawAssistant, ReasoningRetained: true,
		Completion: continuation.Completion{
			StopReason: "stop", ProviderStateType: "meeting-fixture/native.v1",
			ProviderState: json.RawMessage(
				`{"content":"portable differs","tool_calls":[{"name":"native-only"}]}`,
			),
		},
	}
	// Result deliberately overtakes the text terminal on its independent graph
	// edge. The quarantine must retain it until the real TextEnd crosses safe_text.
	pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
		"foreground-result", "result", 0, result)
	for _, frame := range []struct {
		port     string
		sequence uint64
		payload  any
	}{
		{"text_out", 9, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd, Index: 5,
		}},
		{"audio_out", 10, speechelements.AudioFrame{
			Kind: speechelements.AudioEnd, UtteranceID: utterance.ID,
			Terminal: speechelements.SynthesisOutcome{
				UtteranceID: utterance.ID, Kind: speechelements.OutcomeSucceeded,
			},
		}},
		{"outcome", 11, cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
			ProviderReference: meetinggraph.ForegroundDeploymentReference,
		}},
	} {
		pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
			fmt.Sprintf("foreground-response-%d", frame.sequence), frame.port, frame.sequence, frame.payload)
	}

	wantSafeText := "Normal.\nTerminal ordinary <tool"
	var plan v1.SpeechPlan
	select {
	case plan = <-ttsPlans:
	case <-time.After(2 * time.Second):
		t.Fatalf("mounted Meeting graph TTS received no safe plan; events=%v", sink.snapshot())
	}
	if plan.Text != wantSafeText || !strings.HasPrefix(plan.CandidateID,
		"foreground_segment:"+runID+":speech:1") {
		t.Fatalf("mounted Meeting graph TTS plan = %+v, want only %q", plan, wantSafeText)
	}
	awaitMeetingEdgeDequeued(t, native, "foreground.audio_out", "foreground_native_audio_drop.in")
	for _, event := range sink.snapshot() {
		if event == "audio" || event == "speech_end" || event == "turn_end" {
			t.Fatalf("Meeting terminal escaped while graph TTS was blocked: %v", sink.snapshot())
		}
	}
	close(ttsRelease)

	select {
	case <-sink.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("mounted Meeting response did not terminalize; events=%v", sink.snapshot())
	}
	want := []string{
		"turn_begin", "speech_begin", "text:Normal.\n",
		"text:Terminal ordinary ", "text:<tool", "audio", "speech_end", "turn_end",
	}
	if events := sink.snapshot(); !slices.Equal(events, want) {
		t.Fatalf("mounted Meeting safe events = %v, want %v", events, want)
	}
	if strings.Contains(strings.Join(sink.snapshot(), ""), "never-speak") ||
		strings.Contains(strings.Join(sink.snapshot(), ""), "native-only") ||
		strings.Contains(plan.Text, "never-speak") || strings.Contains(plan.Text, "native-only") {
		t.Fatalf("serialized control crossed mounted Meeting adapter: %v", sink.snapshot())
	}
	if audio, utterances := sink.audioSnapshot(); len(audio) != 1 ||
		!slices.Equal(audio[0], graphPCM) || slices.Equal(audio[0], []byte{1, 0, 2, 0}) ||
		!slices.Equal(utterances, []string{wantSafeText}) {
		t.Fatalf("Meeting sink audio/utterance = %v / %v, want graph TTS PCM / %q",
			audio, utterances, wantSafeText)
	}
}

func TestMountedMeetingToolOnlyForegroundCompletesWithoutSpeechFraming(t *testing.T) {
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	created := make(chan *fixtureExternalSession, 1)
	ttsPlans := make(chan v1.SpeechPlan, 1)
	_ = installValidMountServices(t, &fixture, &modelDials, &visualFactories, &visualCloses,
		func(_ context.Context, hello sidecar.Message) (modelelements.Session, error) {
			session := newFixtureExternalSession(hello)
			created <- session
			return session, nil
		}, fixtureVisualConfig{ttsPlans: ttsPlans})
	realAdapter := meetinggraph.SessionAdapterFactory(meetinggraph.SessionAdapterConfig{})
	fixture.config.Adapter.Factory = func(
		ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		return realAdapter(ctx, mounted, options, profile)
	}
	provider, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	sink := &quarantineMeetingSink{done: make(chan struct{})}
	const sessionID = "meeting-tool-only-mounted"
	runtime, err := provider.Binding.Start(context.Background(), legacy.Options{
		SessionID: sessionID, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*graphbinding.NativeRuntime)
	if !ok {
		t.Fatalf("mounted Meeting runtime has type %T", runtime)
	}
	defer func() {
		if closeErr := runtime.Close(context.Background(), errors.New("test complete")); closeErr != nil {
			t.Errorf("close mounted Meeting runtime: %v", closeErr)
		}
	}()

	var external *fixtureExternalSession
	select {
	case external = <-created:
		external.leaveFramesOpen = true
	case <-time.After(2 * time.Second):
		t.Fatal("mounted Meeting foreground deployment was not dialed")
	}
	const runID = "foreground-tool-only-run"
	tool := continuation.ToolDefinition{
		Name: "lookup", Description: "look up one record",
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	}
	proposal := cognitionelements.ToolProposal{
		Call: trajectory.ToolCall{
			CallID: "foreground-tool-only-call", Name: tool.Name,
			Arguments: json.RawMessage(`{"id":"record-1"}`),
		},
		Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
	}
	result := cognitionelements.Result{
		RunID: runID, ProviderReference: meetinggraph.ForegroundDeploymentReference,
		Descriptor: continuation.Descriptor{
			Provider: "meeting-fixture", Model: "foreground", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
			ToolAuthority:   continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
		},
		Invocation: continuation.Invocation{Instruction: "use the lookup", Tools: []continuation.ToolDefinition{tool}},
		Outputs: []cognitionelements.PreparedOutput{{
			Kind: cognitionelements.PreparedTool, Proposal: &proposal,
		}},
		ToolProposals: []cognitionelements.ToolProposal{proposal},
		Completion:    continuation.Completion{StopReason: "tool_call"},
	}
	pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
		"foreground-tool-only-proposal", "tool_proposal", 1, proposal)
	pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
		"foreground-tool-only-result", "result", 0, result)
	pushMeetingForegroundFrame(t, external, native.Done(), sessionID, runID,
		"foreground-tool-only-outcome", "outcome", 2, cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
			ProviderReference: meetinggraph.ForegroundDeploymentReference,
		})

	select {
	case <-sink.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("tool-only Meeting response did not terminalize; events=%v", sink.snapshot())
	}
	want := []string{"turn_begin", "tool:foreground-tool-only-call", "turn_end"}
	if events := sink.snapshot(); !slices.Equal(events, want) {
		t.Fatalf("tool-only Meeting events = %v, want %v", events, want)
	}
	select {
	case plan := <-ttsPlans:
		t.Fatalf("tool-only Meeting unexpectedly reached graph TTS: %+v", plan)
	default:
	}
}

type mountedMeetingCancellationHarness struct {
	runtime   legacy.Runtime
	native    *graphbinding.NativeRuntime
	mounted   *graphruntime.Mounted
	external  *fixtureExternalSession
	sink      *quarantineMeetingSink
	sessionID string
}

func mountMeetingCancellationHarness(
	t *testing.T, sessionID string,
) mountedMeetingCancellationHarness {
	t.Helper()
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	created := make(chan *fixtureExternalSession, 1)
	_ = installValidMountServices(t, &fixture, &modelDials, &visualFactories, &visualCloses,
		func(_ context.Context, hello sidecar.Message) (modelelements.Session, error) {
			session := newFixtureExternalSession(hello)
			created <- session
			return session, nil
		}, fixtureVisualConfig{})
	realAdapter := meetinggraph.SessionAdapterFactory(meetinggraph.SessionAdapterConfig{})
	var mountedGraph *graphruntime.Mounted
	fixture.config.Adapter.Factory = func(
		ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		mountedGraph = mounted
		return realAdapter(ctx, mounted, options, profile)
	}
	provider, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	sink := &quarantineMeetingSink{done: make(chan struct{})}
	runtime, err := provider.Binding.Start(context.Background(), legacy.Options{
		SessionID: sessionID, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*graphbinding.NativeRuntime)
	if !ok || mountedGraph == nil {
		t.Fatalf("mounted Meeting runtime = %T, graph = %p", runtime, mountedGraph)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = runtime.Close(ctx, errors.New("test complete"))
	})
	var external *fixtureExternalSession
	select {
	case external = <-created:
		external.leaveFramesOpen = true
	case <-time.After(2 * time.Second):
		t.Fatal("mounted Meeting foreground deployment was not dialed")
	}
	return mountedMeetingCancellationHarness{
		runtime: runtime, native: native, mounted: mountedGraph, external: external,
		sink: sink, sessionID: sessionID,
	}
}

func (harness mountedMeetingCancellationHarness) awaitExactForegroundCancel(
	t *testing.T, runID, reason string,
) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case message := <-harness.external.sent:
			if message.Port != "cancel" {
				continue
			}
			if message.Envelope == nil {
				t.Fatal("Meeting foreground cancellation has no envelope")
			}
			var cancel cognitionelements.Cancel
			if err := json.Unmarshal(message.Envelope.JSON, &cancel); err != nil {
				t.Fatal(err)
			}
			if message.Envelope.RunID != runID || message.Envelope.CancellationScope != runID ||
				cancel.RunID != runID || cancel.Reason != reason ||
				!strings.HasSuffix(message.Envelope.ItemID, ":model-cancel") {
				t.Fatalf("Meeting foreground cancellation lost exact addressing: message=%+v payload=%+v",
					message, cancel)
			}
			return
		case <-harness.native.Done():
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := harness.runtime.Close(ctx, errors.New("inspect premature stop"))
			cancel()
			t.Fatalf("mounted Meeting runtime stopped before canceling run %q: %v", runID, err)
		case <-deadline:
			t.Fatalf("segmentation failure did not cancel foreground run %q; live=%+v",
				runID, harness.native.Live())
		}
	}
}

func (harness mountedMeetingCancellationHarness) timeoutSegmentation(
	t *testing.T, runID, reason string,
) {
	t.Helper()
	port, err := harness.mounted.Ingress("speech_timeout")
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := port.Broadcast(context.Background(), element.Envelope{
		Type: port.Type(), ItemID: "meeting-timeout:" + runID,
		SessionID: harness.sessionID, SourceID: "meeting.timeout", RunID: runID,
		CancellationScope: runID,
		Payload:           interactionelements.Timeout{RunID: runID, Reason: reason},
	})
	if err != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		t.Fatalf("send Meeting segmentation timeout = %+v, %v", delivery, err)
	}
}

func TestMountedMeetingSegmentationFailureCancelsExactForegroundRun(t *testing.T) {
	t.Run("size", func(t *testing.T) {
		harness := mountMeetingCancellationHarness(t, "meeting-segment-size-cancel")
		const runID = "foreground-segment-size-run"
		pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
			"foreground-segment-size-begin", "text_out", 1, cognitionelements.PreparedTextDelta{
				Boundary: cognitionelements.TextBegin, Index: 0,
			})
		pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
			"foreground-segment-size-chunk", "text_out", 2, cognitionelements.PreparedTextDelta{
				Boundary: cognitionelements.TextChunk, Index: 1, Text: strings.Repeat("x", 4097),
			})
		harness.awaitExactForegroundCancel(t, runID, "prepared run exceeds 4096 bytes")
	})

	t.Run("timeout", func(t *testing.T) {
		harness := mountMeetingCancellationHarness(t, "meeting-segment-timeout-cancel")
		const runID = "foreground-segment-timeout-run"
		harness.timeoutSegmentation(t, runID, "foreground speech deadline")
		harness.awaitExactForegroundCancel(t, runID, "foreground speech deadline")
	})
}

func TestMountedMeetingSegmentationFailureRejectsBufferedForegroundProposal(t *testing.T) {
	harness := mountMeetingCancellationHarness(t, "meeting-segment-buffered-proposal")
	const runID = "foreground-segment-buffered-proposal-run"
	pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
		"buffered-tool-proposal", "tool_proposal", 1, cognitionelements.ToolProposal{
			Call: trajectory.ToolCall{
				CallID: "buffered-call", Name: "lookup",
				Arguments: json.RawMessage(`{"id":"must-stay-quarantined"}`),
			},
			Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
		})
	pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
		"buffered-proposal-size-begin", "text_out", 2, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin, Index: 0,
		})
	pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
		"buffered-proposal-size-chunk", "text_out", 3, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 1, Text: strings.Repeat("x", 4097),
		})
	harness.awaitExactForegroundCancel(t, runID, "prepared run exceeds 4096 bytes")
	pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
		"buffered-proposal-canceled", "outcome", 4, cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: runID,
			ProviderReference: meetinggraph.ForegroundDeploymentReference,
			Code:              "canceled", Message: "segmentation canceled foreground",
		})

	select {
	case <-harness.sink.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("failed Meeting response did not terminalize; events=%v live=%+v",
			harness.sink.snapshot(), harness.native.Live())
	}
	if events := harness.sink.snapshot(); !slices.Equal(events, []string{"turn_begin", "turn_end"}) {
		t.Fatalf("buffered proposal escaped failed segmentation: %v", events)
	}
}

func TestMountedMeetingRejectsForegroundOutputAfterSegmentationCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		push func(*testing.T, mountedMeetingCancellationHarness, string)
	}{
		{
			name: "tool proposal",
			push: func(t *testing.T, harness mountedMeetingCancellationHarness, runID string) {
				pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
					"late-tool-proposal", "tool_proposal", 1, cognitionelements.ToolProposal{
						Call: trajectory.ToolCall{
							CallID: "late-call", Name: "lookup",
							Arguments: json.RawMessage(`{"id":"must-not-escape"}`),
						},
						Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
					})
			},
		},
		{
			name: "safe result source",
			push: func(t *testing.T, harness mountedMeetingCancellationHarness, runID string) {
				pushMeetingForegroundFrame(t, harness.external, harness.native.Done(), harness.sessionID, runID,
					"late-result", "result", 1, cognitionelements.Result{
						RunID: runID, ProviderReference: meetinggraph.ForegroundDeploymentReference,
						Descriptor: continuation.Descriptor{
							Provider: "meeting-fixture", Model: "foreground", Phase: trajectory.PhaseFast,
							Effort: continuation.EffortMinimal, Streaming: true,
							ToolAuthority:   continuation.ToolAuthorityPropose,
							SpeechAuthority: continuation.SpeechAuthorityVoice,
						},
						Invocation: continuation.Invocation{Instruction: "late output"},
						Completion: continuation.Completion{StopReason: "ignored_cancel"},
					})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "foreground-timeout-late-" + strings.ReplaceAll(test.name, " ", "-")
			harness := mountMeetingCancellationHarness(t, "meeting-timeout-late-"+strings.ReplaceAll(test.name, " ", "-"))
			harness.timeoutSegmentation(t, runID, "foreground speech deadline")
			harness.awaitExactForegroundCancel(t, runID, "foreground speech deadline")
			test.push(t, harness, runID)

			select {
			case <-harness.native.Done():
			case <-time.After(2 * time.Second):
				t.Fatalf("late %s was not rejected; events=%v live=%+v",
					test.name, harness.sink.snapshot(), harness.native.Live())
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := harness.runtime.Close(ctx, errors.New("inspect expected rejection"))
			if err == nil || !strings.Contains(err.Error(), "after segmentation cancellation") {
				t.Fatalf("late %s rejection error = %v", test.name, err)
			}
			for _, event := range harness.sink.snapshot() {
				if strings.HasPrefix(event, "tool:") || strings.HasPrefix(event, "text:") ||
					event == "speech_begin" || event == "audio" {
					t.Fatalf("late %s escaped the Meeting quarantine: %v", test.name, harness.sink.snapshot())
				}
			}
		})
	}
}

const toolCallOpenForMeetingTest = "<tool_call>"

func pushMeetingForegroundFrame(
	t *testing.T, session *fixtureExternalSession, runtimeDone <-chan struct{},
	sessionID, runID, itemID, port string,
	sequence uint64, payload any,
) {
	t.Helper()
	valueType := modelelements.PreparedTextType()
	switch port {
	case "audio_out":
		valueType = modelelements.PreparedAudioType()
	case "tool_proposal":
		valueType = modelelements.ToolProposalType()
	case "result":
		valueType = cognitionelements.ResultType()
	case "outcome":
		valueType = modelelements.OutcomeType()
	case "text_out":
	default:
		t.Fatalf("unknown foreground fixture port %q", port)
	}
	encoded, err := modelelements.NewStandardJSONCodec().Encode(valueType, payload)
	if err != nil {
		t.Fatalf("encode foreground fixture %s: %v", port, err)
	}
	message := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: port,
		Envelope: &sidecar.WireEnvelope{
			Type: valueType, ItemID: itemID, SessionID: sessionID,
			SourceID: meetinggraph.ForegroundDeploymentReference, RunID: runID,
			Sequence: sequence, JSON: encoded.JSON, Media: encoded.Media,
		},
		Payload: encoded.Binary, PayloadBytes: len(encoded.Binary),
	}
	select {
	case session.frames <- message:
	case <-runtimeDone:
		t.Fatalf("mounted Meeting runtime stopped before foreground %s sequence %d", port, sequence)
	case <-time.After(2 * time.Second):
		t.Fatalf("mounted Meeting foreground %s sequence %d was not received", port, sequence)
	}
}

func TestMeetingGraphRunsThroughRealtimeWebSocketWithoutCredentials(t *testing.T) {
	fixture := newProviderFixture(t)
	var modelDials, visualFactories, visualCloses atomic.Int64
	created := make(chan *fixtureExternalSession, 1)
	mountedSessions := make(chan *graphruntime.Mounted, 1)
	narrated := make(chan perception.Frame, 2)
	backgroundRequests := installValidMountServices(
		t, &fixture, &modelDials, &visualFactories, &visualCloses,
		func(_ context.Context, hello sidecar.Message) (modelelements.Session, error) {
			session := newFixtureExternalSession(hello)
			created <- session
			return session, nil
		}, fixtureVisualConfig{frames: narrated, text: "The shared screen changed to the overview slide."},
	)
	fixture.config.Adapter.Factory = func(
		_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixture.adapterFactories.Add(1)
		if mounted == nil || profile.GraphFingerprint != mounted.Graph().Fingerprint {
			return nil, errors.New("adapter received a drifted mounted graph")
		}
		select {
		case mountedSessions <- mounted:
		default:
			return nil, errors.New("fixture received more than one meeting session")
		}
		return newFixtureSessionAdapter(mounted, options)
	}
	application := newMeetingApplicationFixture(t, fixture)
	registration, err := meetinggraph.NewApplicationRegistration(application.host)
	if err != nil {
		t.Fatal(err)
	}
	profile, previewPlan := freezeMeetingLaunchProfile(t, registration, application)
	applications, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	profiled, err := serverplugin.NewProfileGraphBundle(context.Background(), serverplugin.ProfileGraphBundleConfig{
		Profile: profile, Applications: applications,
		GatewayArtifact: application.gatewayArtifact,
		Gateway:         gateway.Config{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profiled.GraphPlan.Identity() != previewPlan.Identity() {
		t.Fatalf("profiled meeting plan drifted: got %+v want %+v",
			profiled.GraphPlan.Identity(), previewPlan.Identity())
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 ||
		modelDials.Load() != 0 || visualFactories.Load() != 0 {
		t.Fatalf("profile composition acquired mount=%d adapter=%d model=%d visual=%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load(),
			modelDials.Load(), visualFactories.Load())
	}
	realm, err := profiled.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 ||
		modelDials.Load() != 0 || visualFactories.Load() != 0 {
		t.Fatalf("server realm mount acquired graph session mount=%d adapter=%d model=%d visual=%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load(),
			modelDials.Load(), visualFactories.Load())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close meeting server realm: %v", err)
		}
	})
	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	connection, _, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime?model=meeting-graph-fixture",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "meeting fixture complete")
	})
	if createdEvent := awaitMeetingWebSocketEvent(t, connection, "session.created"); createdEvent["type"] != "session.created" {
		t.Fatalf("meeting session creation = %+v", createdEvent)
	}
	var mounted *graphruntime.Mounted
	select {
	case mounted = <-mountedSessions:
	case <-time.After(5 * time.Second):
		t.Fatal("meeting WebSocket did not start a graph-native session")
	}
	if fixture.mountFactories.Load() != 5 || fixture.adapterFactories.Load() != 1 {
		t.Fatalf("meeting session Start acquired mount=%d adapter=%d, want 5/1",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load())
	}
	var external *fixtureExternalSession
	select {
	case external = <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("meeting WebSocket did not dial the foreground deployment")
	}
	writeMeetingWebSocketEvent(t, connection, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				},
			},
			"openrealtime": map[string]any{
				"version":   openrealtime.Version,
				"supports":  []string{"video.input", "observations"},
				"observers": []string{"screen"},
			},
		},
	})
	updated := awaitMeetingWebSocketEvent(t, connection, "session.updated")
	updatedSession, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("meeting session.updated = %+v", updated)
	}
	extension, ok := updatedSession["openrealtime"].(map[string]any)
	if !ok || !fixtureJSONStringSet(extension["enabled"], "video.input", "observations") ||
		!fixtureJSONStringSet(extension["observers"], "screen") {
		t.Fatalf("meeting extension negotiation = %+v", extension)
	}
	writeMeetingWebSocketEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": 32, "height": 24,
	})
	firstFrame := fixtureMeetingJPEG(t, 0x11)
	secondFrame := fixtureMeetingJPEG(t, 0xee)
	writeMeetingWebSocketEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame":        base64.StdEncoding.EncodeToString(firstFrame),
		"timestamp_ms": 1000,
	})
	// The public realm advertises three frames per second. Waiting across that
	// exact admission interval keeps the second frame in the graph rather than
	// accidentally testing the gateway's intentional rate-drop path.
	time.Sleep(350 * time.Millisecond)
	writeMeetingWebSocketEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame":        base64.StdEncoding.EncodeToString(secondFrame),
		"timestamp_ms": 1200,
	})
	// Frame and cadence signals are explicit independent graph inputs. A third
	// admitted source frame supplies the next clock edge if the second tick won
	// fair input arbitration before its matching frame reached the policy.
	time.Sleep(350 * time.Millisecond)
	writeMeetingWebSocketEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame":        base64.StdEncoding.EncodeToString(fixtureMeetingJPEG(t, 0x57)),
		"timestamp_ms": 1400,
	})
	select {
	case frame := <-narrated:
		if frame.Source != "screen" || frame.Index < 1 || frame.Index > 3 ||
			(frame.CapturedNS != 1000*uint64(time.Millisecond) &&
				frame.CapturedNS != 1200*uint64(time.Millisecond) &&
				frame.CapturedNS != 1400*uint64(time.Millisecond)) {
			t.Fatalf("adaptive meeting observer frame = %+v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("screen input did not reach adaptive observation; live=%+v", mounted.Live())
	}
	observation := awaitMeetingWebSocketEvent(t, connection, openrealtime.EventObservationAdded)
	if observation["text"] != "The shared screen changed to the overview slide." ||
		observation["observer"] != "screen" || observation["source"] != "screen" ||
		observation["authority"] != string(trajectory.AuthorityObserver) {
		t.Fatalf("meeting WebSocket observation = %+v", observation)
	}
	select {
	case request := <-backgroundRequests:
		if request.Trajectory.Version != 1 || request.Invocation.Instruction == "" {
			t.Fatalf("meeting background request = %+v", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("screen observation did not activate background cognition; live=%+v", mounted.Live())
	}
	seenVideos, textOrder, order := 0, 0, 0
	var injectionFrame sidecar.Message
	// Thirty seconds bounds "these never arrive", not how quickly they do. What
	// is under test is that three video frames and the retained background
	// context reach the foreground at all, and in the right order; the graph's
	// latency is what the measurement suites report, and asserting it here
	// would be asserting it on whatever machine happened to run the gate. Five
	// seconds was tight enough to lose that distinction on a loaded CI runner,
	// where this arrived one frame short and failed as though the handoff were
	// broken.
	deadline := time.After(30 * time.Second)
	for seenVideos < 3 || injectionFrame.Envelope == nil {
		select {
		case message := <-external.sent:
			order++
			switch message.Port {
			case "video":
				seenVideos++
				if len(message.Payload) == 0 || message.Envelope == nil || message.Envelope.CaptureNS == 0 {
					t.Fatalf("direct foreground video = %+v", message)
				}
			case "text":
				if injectionFrame.Envelope == nil {
					injectionFrame, textOrder = message, order
				}
			case "trigger":
				t.Fatalf("background completion autonomously reached foreground trigger: %+v", message)
			}
		case <-deadline:
			t.Fatalf("meeting foreground handoff video=%d text=%t live=%+v",
				seenVideos, injectionFrame.Envelope != nil, mounted.Live())
		}
	}
	if textOrder == 0 {
		t.Fatalf("foreground provider did not receive retained background context: %+v", injectionFrame)
	}
	awaitMeetingEdgeDequeued(t, mounted, "background_injection.trigger", "background_trigger_drop.in")
	awaitMeetingEdgeDequeued(t, mounted, "background_injection.outcome", "background_outcome_copy.in")
	writeMeetingWebSocketEvent(t, connection, map[string]any{"type": "response.cancel"})
	for {
		select {
		case message := <-external.sent:
			if message.Port != "cancel" {
				continue
			}
			if message.Envelope == nil || message.Envelope.RunID == "" {
				t.Fatalf("meeting cancellation frame = %+v", message)
			}
			var cancel cognitionelements.Cancel
			if err := json.Unmarshal(message.Envelope.JSON, &cancel); err != nil {
				t.Fatal(err)
			}
			if cancel.RunID == "" || cancel.Reason != "client response cancellation" {
				t.Fatalf("meeting cancellation payload = %+v", cancel)
			}
			goto cancelled
		case <-time.After(5 * time.Second):
			t.Fatalf("Realtime response.cancel did not reach the foreground; live=%+v", mounted.Live())
		}
	}

cancelled:
	if graph := mounted.Live(); graph.Fingerprint != profiled.GraphPlan.Graph().Fingerprint {
		t.Fatalf("meeting WebSocket graph identity = %+v", graph)
	}
}

func TestMeetingProviderRejectsIncompleteRealtimeProjectionBeforeLaunch(t *testing.T) {
	fixture := newProviderFixture(t)
	fixture.config.Adapter.Profile.Stack.TextInjection = false
	_, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err == nil || !strings.Contains(err.Error(), "text injection capability") {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("rejected adapter acquired a runtime resource")
	}
}

func TestMeetingProviderRejectsNonCanonicalObserverBeforeLaunch(t *testing.T) {
	fixture := newProviderFixture(t)
	fixture.config.Adapter.Profile.AdditionalObservers = []string{" secondary-screen"}
	_, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err == nil || !strings.Contains(err.Error(), "observer must be canonical") {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("non-canonical observer acquired a runtime resource")
	}
}

func TestMeetingProviderRejectsProviderReferenceDriftBeforeLaunch(t *testing.T) {
	fixture := newProviderFixture(t)
	fixture.config.Artifacts.Values.Data = bytes.ReplaceAll(
		fixture.config.Artifacts.Values.Data,
		[]byte(meetinggraph.VisualProviderReference),
		[]byte("meeting.visual-drift"),
	)
	_, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err == nil || !strings.Contains(err.Error(), `screen_observer provider reference "meeting.visual-drift"`) {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("provider-reference drift acquired a runtime resource")
	}
}

func TestMeetingProviderHonorsCancellationBeforePluginSelection(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("meeting launch cancelled")
	cancel(want)
	_, err := meetinggraph.NewProvider(ctx, fixture.config)
	if !errors.Is(err, want) {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("cancelled provider construction acquired a runtime resource")
	}
}

func TestMeetingAdapterSnapshotsAdditionalObservers(t *testing.T) {
	fixture := newProviderFixture(t)
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	configuration := fixture.config.Adapter
	configuration.Profile.AdditionalObservers = []string{"secondary-screen"}
	plugin, _, err := meetinggraph.AdapterPlugin(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Profile.AdditionalObservers[0] = "mutated-after-construction"
	bound, err := plugin.Bind(context.Background(), result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	observers := bound.Profile.Capabilities.Observers
	if len(observers) != 2 || observers[0] != "screen" || observers[1] != "secondary-screen" {
		t.Fatalf("meeting observer snapshot = %v", observers)
	}
}

func TestMeetingBundleLockIsExactAndMinimal(t *testing.T) {
	fixture := newProviderFixture(t)
	result, err := meetinggraph.NewProvider(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Lock().Entries) != 17 {
		t.Fatalf("meeting lock entries = %d, want 17", len(result.Plan.Lock().Entries))
	}
	for _, name := range []string{
		"audio", "video", "text", "tools", "tool_result", "commit_audio",
		"create_response", "cancel", "truncate", "transcript", "observations",
		"prepared_text", "prepared_audio", "tool_proposals", "foreground_outcome",
		"foreground_safe_result", "foreground_model_cancel_request", "foreground_segmentation_outcome",
		"foreground_synthesis_outcome", "background_outcome",
	} {
		if !hasBoundary(result.Plan.Graph(), name) {
			t.Fatalf("meeting graph has no boundary %q", name)
		}
	}
	graph := result.Plan.Graph()
	for _, boundary := range graph.Boundaries {
		switch boundary.Name {
		case "prepared_text":
			if boundary.Direction != ir.OutputBoundary ||
				!boundary.Type.Equal(interactionelements.SafePreparedTextType()) {
				t.Fatalf("meeting prepared_text boundary = %s %s, want output %s",
					boundary.Direction, boundary.Type.String(),
					interactionelements.SafePreparedTextType().String())
			}
		case "prepared_audio":
			if boundary.Direction != ir.OutputBoundary ||
				!boundary.Type.Equal(speechelements.AudioType()) ||
				boundary.Endpoint.Node != "foreground_tts" || boundary.Endpoint.Port != "audio" {
				t.Fatalf("meeting prepared_audio boundary = %+v, want foreground_tts.audio %s",
					boundary, speechelements.AudioType().String())
			}
		case "foreground_model_cancel_request":
			if boundary.Direction != ir.OutputBoundary ||
				!boundary.Type.Equal(interactionelements.ModelCancelType()) ||
				boundary.Endpoint.Node != "foreground_segment" || boundary.Endpoint.Port != "model_cancel" {
				t.Fatalf("meeting model-cancel boundary = %+v, want foreground_segment.model_cancel %s",
					boundary, interactionelements.ModelCancelType().String())
			}
		}
	}
	for _, edge := range []struct {
		from, to string
		delivery ir.Delivery
	}{
		{"screen_fork.foreground", "foreground.video", ir.Lossy},
		{"screen_fork.frame", "screen_ingress.frame_in", ir.Lossy},
		{"screen_fork.tick", "screen_policy.tick", ir.Lossless},
		{"background_model.text", "background_control_quarantine.text", ir.Lossless},
		{"background_model.result", "background_control_quarantine.result", ir.Lossless},
		{"background_control_quarantine.safe_text", "background_injection.text", ir.Lossless},
		{"background_control_quarantine.safe_result", "background_result_copy.in", ir.Lossless},
		{"background_injection.injection", "text_injection_mux.in", ir.Lossless},
		{"background_injection.trigger", "background_trigger_drop.in", ir.Lossless},
		{"foreground.text_out", "foreground_control_quarantine.text", ir.Lossless},
		{"foreground.result", "foreground_control_quarantine.result", ir.Lossless},
		{"foreground_control_quarantine.safe_text", "foreground_safe_text_copy.in", ir.Lossless},
		{"foreground_safe_text_copy.out", "foreground_segment.text", ir.Lossless},
		{"foreground_segment.segments", "foreground_tts.text", ir.Lossless},
		{"foreground.audio_out", "foreground_native_audio_drop.in", ir.Lossless},
		{"foreground_control_quarantine.safe_result", "foreground_result_copy.in", ir.Lossless},
		{"foreground_control_quarantine.quarantined", "foreground_control_quarantine_audit.in", ir.Lossless},
		{"background_control_quarantine.quarantined", "background_control_quarantine_audit.in", ir.Lossless},
		{"foreground_result_commit.outcome", "foreground_commit_audit.in", ir.Lossless},
		{"background_model.resolved", "background_model_resolution_audit.in", ir.Lossless},
		{"screen_observer.metrics", "screen_visual_metrics_audit.in", ir.Lossless},
	} {
		if !hasEdge(graph, edge.from, edge.to, edge.delivery) {
			t.Fatalf("meeting graph has no %s edge %s -> %s", edge.delivery, edge.from, edge.to)
		}
	}
	if hasEdge(graph, "background_injection.trigger", "foreground_trigger_mux.in", ir.Lossless) {
		t.Fatal("background completion can autonomously open a voiced foreground turn")
	}
	for _, bypass := range [][2]string{
		{"background_model.text", "background_injection.text"},
		{"background_model.result", "background_result_copy.in"},
		{"foreground.result", "foreground_result_copy.in"},
	} {
		if hasEdge(graph, bypass[0], bypass[1], ir.Lossless) {
			t.Fatalf("meeting graph bypasses control serialization quarantine: %s -> %s",
				bypass[0], bypass[1])
		}
	}
	allowedOutputs := map[string]struct{}{
		"transcript": {}, "observations": {}, "activity": {}, "prepared_text": {},
		"prepared_audio": {}, "tool_proposals": {}, "foreground_outcome": {},
		"foreground_safe_result": {}, "foreground_model_cancel_request": {},
		"foreground_segmentation_outcome": {},
		"foreground_synthesis_outcome":    {}, "background_outcome": {},
	}
	for _, boundary := range graph.Boundaries {
		if boundary.Direction == ir.OutputBoundary {
			if _, projected := allowedOutputs[boundary.Name]; !projected {
				t.Fatalf("meeting graph exposes unprojected output boundary %q", boundary.Name)
			}
		}
	}
	var foreground struct {
		Required []sidecar.CapabilityRequirement `json:"required_capabilities"`
	}
	if err := json.Unmarshal(result.Plan.Values()["foreground"], &foreground); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(foreground.Required, sidecar.CapabilityRequirement{
		Name:     meetinggraph.ForegroundCausalOrderingCapability,
		Contract: meetinggraph.ForegroundCausalOrderingContract,
	}) {
		t.Fatalf("foreground requirements = %+v", foreground.Required)
	}
}

type providerFixture struct {
	config           meetinggraph.ProviderConfig
	mountFactories   *atomic.Int64
	adapterFactories *atomic.Int64
}

func newProviderFixture(t testing.TB) providerFixture {
	t.Helper()
	root := filepath.Join("..", "..", "graphs", "components", "meeting-assistant")
	read := func(name string) []byte {
		t.Helper()
		payload, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	secrets, err := graphsecret.ParseYAML("agent.secrets.yaml", read("agent.secrets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := graphevidence.ParseYAML("agent.evidence.yaml", read("agent.evidence.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mountCalls := &atomic.Int64{}
	adapterCalls := &atomic.Int64{}
	plugins := graphlaunch.Catalog{}
	for _, name := range []string{
		modelelements.DeploymentRegistryService,
		modelelements.PayloadCodecService,
		perceptionelements.VisualProviderRegistryService,
		cognitionelements.ProviderRegistryService,
		speechelements.TTSProviderRegistryService,
	} {
		artifact := dependencyArtifact(name)
		plugins.Assembly.Dependencies = append(plugins.Assembly.Dependencies,
			graphassembly.Dependency{Name: name, Artifact: artifact, Scope: graphconfig.DependencyScopeMount},
		)
		plugins.MountDependencies = append(plugins.MountDependencies,
			graphlaunch.MountDependencyPlugin{
				Name: name, Artifact: artifact,
				Factory: func(_ context.Context, _ legacy.Options) ([]graphruntime.PreparedMountDependency, error) {
					mountCalls.Add(1)
					return []graphruntime.PreparedMountDependency{{
						Name: name, Artifact: artifact, Service: struct{}{},
					}}, nil
				},
			},
		)
	}
	config := meetinggraph.ProviderConfig{
		GraphMetadata: graphcatalog.Metadata{
			Stage: graphcatalog.Experimental, Summary: "Meeting Assistant test graph.",
			Change: "Initial exact test graph revision.", Tags: []string{"meeting", "test"},
		},
		Artifacts: graphconfig.Artifacts{
			Topology:   graphconfig.Artifact{Path: "agent.ortg", Encoding: graphconfig.ORTG, Data: read("agent.ortg")},
			Values:     graphconfig.Artifact{Path: "agent.values.yaml", Encoding: graphconfig.YAML, Data: read("agent.values.yaml")},
			Lock:       graphconfig.Artifact{Path: "openrealtime.lock", Encoding: graphconfig.JSON, Data: read("openrealtime.lock")},
			Deployment: graphconfig.Artifact{Path: "agent.deployment.yaml", Encoding: graphconfig.YAML, Data: read("agent.deployment.yaml")},
		},
		PlanOptions: graphconfig.Options{Revision: 1}, Plugins: plugins,
		SecretCatalog: &secrets, Evidence: evidence,
		Adapter: meetinggraph.AdapterPluginConfig{
			Reference: "go://openrealtime/meeting-adapters/cascade/v1",
			Artifact:  inspect.ArtifactIdentity{ID: "go://openrealtime/meeting-adapters/cascade", Revision: "build-1"},
			Profile: meetinggraph.AdapterProfileConfig{
				Name: "openrealtime.meeting.cascade", Revision: 1,
				Ownership: legacy.Ownership{
					Perception: legacy.OwnerModel, FastCognition: legacy.OwnerModel,
					SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerModel,
					Interaction: legacy.OwnerModel, Floor: legacy.OwnerModel,
				},
				Voice: legacy.VoiceControl{Selectable: false, InForce: "meeting-default"},
				Stack: legacy.StackCapabilities{
					AudioInput: true, AudioOutput: true, VisualInput: true,
					Transcription: true, TurnGeneration: true, ConcurrentIO: true,
					TextInjection: true,
				},
			},
			Factory: func(
				context.Context, *graphruntime.Mounted, legacy.Options,
				graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				adapterCalls.Add(1)
				return nil, errors.New("adapter should not be acquired in this fixture")
			},
		},
	}
	return providerFixture{config: config, mountFactories: mountCalls, adapterFactories: adapterCalls}
}

func installValidMountServices(
	t testing.TB, fixture *providerFixture,
	modelDials, visualFactories, visualCloses *atomic.Int64,
	modelDial modelelements.Dialer,
	visual fixtureVisualConfig,
) <-chan continuation.Request {
	t.Helper()
	if modelDial == nil {
		modelDial = func(context.Context, sidecar.Message) (modelelements.Session, error) {
			return nil, errors.New("model must not dial before graph Run")
		}
	}
	deployments := modelelements.NewDeploymentRegistry()
	if err := deployments.Register(meetinggraph.ForegroundDeploymentReference,
		func(ctx context.Context, hello sidecar.Message) (modelelements.Session, error) {
			modelDials.Add(1)
			return modelDial(ctx, hello)
		}); err != nil {
		t.Fatal(err)
	}
	visualDescriptor := perceptionelements.VisualProviderDescriptor{
		Name: "meeting-fixture-visual", Revision: "fixture-1",
	}
	visuals := perceptionelements.NewVisualProviderRegistry()
	if err := visuals.Register(meetinggraph.VisualProviderReference, visualDescriptor,
		func() (perceptionelements.VisualProvider, error) {
			visualFactories.Add(1)
			return &fixtureVisualProvider{
				descriptor: visualDescriptor, closes: visualCloses,
				frames: visual.frames, text: visual.text,
			}, nil
		}); err != nil {
		t.Fatal(err)
	}
	backgroundDescriptor := continuation.Descriptor{
		Provider: "meeting-fixture", Model: "background", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	backgrounds := cognitionelements.NewProviderRegistry()
	backgroundRequests := make(chan continuation.Request, 4)
	if err := backgrounds.Register(meetinggraph.BackgroundProviderReference, backgroundDescriptor,
		func() (continuation.Provider, error) {
			return fixtureContinuation{
				descriptor: backgroundDescriptor, requests: backgroundRequests,
			}, nil
		}); err != nil {
		t.Fatal(err)
	}
	ttsDescriptor := fixtureTTSDescriptor()
	ttsProviders := speechelements.NewTTSProviderRegistry()
	if err := ttsProviders.Register(meetinggraph.TTSProviderReference, ttsDescriptor,
		func() (v1.SpeechProvider, error) {
			return &fixtureTTSProvider{
				descriptor: ttsDescriptor, plans: visual.ttsPlans,
				pcm: slices.Clone(visual.ttsPCM), release: visual.ttsRelease,
			}, nil
		}); err != nil {
		t.Fatal(err)
	}
	services := map[string]any{
		modelelements.DeploymentRegistryService:          deployments,
		modelelements.PayloadCodecService:                modelelements.NewStandardJSONCodec(),
		perceptionelements.VisualProviderRegistryService: visuals,
		cognitionelements.ProviderRegistryService:        backgrounds,
		speechelements.TTSProviderRegistryService:        ttsProviders,
	}
	for index := range fixture.config.Plugins.MountDependencies {
		plugin := &fixture.config.Plugins.MountDependencies[index]
		name, artifact, service := plugin.Name, plugin.Artifact, services[plugin.Name]
		plugin.Factory = func(
			context.Context, legacy.Options,
		) ([]graphruntime.PreparedMountDependency, error) {
			fixture.mountFactories.Add(1)
			return []graphruntime.PreparedMountDependency{{
				Name: name, Artifact: artifact, Service: service,
			}}, nil
		}
	}
	return backgroundRequests
}

type fixtureVisualConfig struct {
	frames     chan<- perception.Frame
	text       string
	ttsPlans   chan<- v1.SpeechPlan
	ttsPCM     []byte
	ttsRelease <-chan struct{}
}

func fixtureTTSDescriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "meeting-fixture-tts", Version: "fixture-1",
		Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
		},
	}
}

type fixtureTTSProvider struct {
	descriptor v1.Descriptor
	plans      chan<- v1.SpeechPlan
	pcm        []byte
	release    <-chan struct{}
}

func (provider *fixtureTTSProvider) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *fixtureTTSProvider) Synthesize(
	ctx context.Context, plan v1.SpeechPlan,
) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

func (provider *fixtureTTSProvider) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	if provider.plans != nil {
		select {
		case provider.plans <- plan:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if provider.release != nil {
		select {
		case <-provider.release:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	pcm := slices.Clone(provider.pcm)
	if len(pcm) == 0 {
		pcm = []byte{9, 0, 8, 0}
	}
	return emit(v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":fixture-audio", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: pcm, Final: true,
	})
}

type fixtureVisualProvider struct {
	descriptor perceptionelements.VisualProviderDescriptor
	closes     *atomic.Int64
	frames     chan<- perception.Frame
	text       string
}

func (provider *fixtureVisualProvider) Name() string { return provider.descriptor.Name }
func (provider *fixtureVisualProvider) Descriptor() perceptionelements.VisualProviderDescriptor {
	return provider.descriptor
}
func (provider *fixtureVisualProvider) Narrate(
	ctx context.Context, frames []perception.Frame, _ trajectory.Snapshot,
) (string, error) {
	if provider.frames != nil && len(frames) != 0 {
		frame := frames[len(frames)-1]
		frame.Image = slices.Clone(frame.Image)
		select {
		case provider.frames <- frame:
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	}
	return provider.text, nil
}
func (provider *fixtureVisualProvider) Close() error {
	provider.closes.Add(1)
	return nil
}

type fixtureContinuation struct {
	descriptor continuation.Descriptor
	requests   chan<- continuation.Request
}

func (provider fixtureContinuation) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider fixtureContinuation) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	select {
	case provider.requests <- request:
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	}
	if err := emit(continuation.Event{
		Kind: continuation.EventAssistantDelta, Text: "grounded background",
	}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "fixture"}, nil
}

type fixtureExternalSession struct {
	ready     sidecar.Message
	frames    chan sidecar.Message
	sent      chan sidecar.Message
	closeOnce sync.Once
	closes    atomic.Int64
	sendMu    sync.Mutex
	seen      map[string]struct{}
	pending   []sidecar.Message
	// A full-mount failure test may retain the producer side long enough to
	// observe NativeRuntime.Done without racing a send against channel close.
	leaveFramesOpen bool
}

func newFixtureExternalSession(hello sidecar.Message) *fixtureExternalSession {
	provider := sidecar.ArtifactIdentity{ID: "provider/meeting-fixture", Revision: "fixture-1"}
	capabilities := make([]sidecar.CapabilityIdentity, 0,
		len(hello.SelectedPorts)+len(hello.RequiredCapabilities))
	negotiated := make([]sidecar.PortNegotiation, 0, len(hello.SelectedPorts))
	for _, selection := range hello.SelectedPorts {
		capabilities = append(capabilities, sidecar.CapabilityIdentity{
			Name:     sidecar.PortCapabilityName(selection.Direction, selection.Name),
			Contract: selection.Type.String(), Provider: provider,
			Adapter: &sidecar.ArtifactIdentity{ID: "adapter/meeting-wire", Revision: "4"},
		})
		negotiated = append(negotiated, sidecar.PortNegotiation{
			Name: selection.Name, Direction: selection.Direction, Format: selection.Formats[0],
		})
	}
	for _, requirement := range hello.RequiredCapabilities {
		capabilities = append(capabilities, sidecar.CapabilityIdentity{
			Name: requirement.Name, Contract: requirement.Contract, Provider: provider,
		})
	}
	descriptor := hello.ElementDescriptor.Clone()
	return &fixtureExternalSession{
		ready: sidecar.Message{
			Type: sidecar.TypeReady, Version: sidecar.VersionElementGraph,
			ElementDescriptor:    &descriptor,
			AppliedConfigDigest:  sidecar.ElementConfigDigest(hello.ElementConfig),
			RuntimeArtifact:      sidecar.ArtifactIdentity{ID: "runtime/meeting-fixture", Revision: "fixture-1"},
			ResolvedCapabilities: capabilities, NegotiatedPorts: negotiated,
		},
		frames: make(chan sidecar.Message), sent: make(chan sidecar.Message, 64),
		seen: make(map[string]struct{}),
	}
}

func (session *fixtureExternalSession) Ready() sidecar.Message         { return session.ready.Clone() }
func (session *fixtureExternalSession) Frames() <-chan sidecar.Message { return session.frames }
func (*fixtureExternalSession) Err() error                             { return nil }
func (session *fixtureExternalSession) Send(message sidecar.Message) error {
	message = message.Clone()
	session.sendMu.Lock()
	defer session.sendMu.Unlock()
	if message.Port == "trigger" && !session.parentsSeen(message) {
		session.pending = append(session.pending, message)
		return nil
	}
	session.sent <- message
	if message.Envelope != nil && message.Envelope.ItemID != "" {
		session.seen[message.Envelope.ItemID] = struct{}{}
	}
	for index := 0; index < len(session.pending); {
		if !session.parentsSeen(session.pending[index]) {
			index++
			continue
		}
		pending := session.pending[index]
		session.pending = append(session.pending[:index], session.pending[index+1:]...)
		session.sent <- pending
	}
	return nil
}

func (session *fixtureExternalSession) parentsSeen(message sidecar.Message) bool {
	if message.Envelope == nil || len(message.Envelope.CausalParents) == 0 {
		return true
	}
	for _, parent := range message.Envelope.CausalParents {
		if !strings.HasSuffix(parent, "/meeting-background-injection") {
			continue
		}
		if _, found := session.seen[parent]; !found {
			return false
		}
	}
	return true
}
func (session *fixtureExternalSession) Close() error {
	session.closeOnce.Do(func() {
		session.closes.Add(1)
		if !session.leaveFramesOpen {
			close(session.frames)
		}
	})
	return nil
}

type fixtureSessionAdapter struct {
	inputs    map[string]element.OutputPort
	outputs   map[string]element.InputPort
	sink      legacy.Sink
	sessionID string
	sequence  atomic.Uint64
}

func newFixtureSessionAdapter(
	mounted *graphruntime.Mounted, options legacy.Options,
) (*fixtureSessionAdapter, error) {
	if mounted == nil || options.Sink == nil || !fixtureCanonical(options.SessionID) {
		return nil, errors.New("fixture meeting adapter requires graph, sink, and canonical session ID")
	}
	adapter := &fixtureSessionAdapter{
		inputs: make(map[string]element.OutputPort), outputs: make(map[string]element.InputPort),
		sink: options.Sink, sessionID: options.SessionID,
	}
	for _, boundary := range mounted.Graph().Boundaries {
		if boundary.Direction != ir.InputBoundary {
			continue
		}
		port, err := mounted.Ingress(boundary.Name)
		if err != nil {
			return nil, err
		}
		adapter.inputs[boundary.Name] = port
	}
	for _, boundary := range mounted.Graph().Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		port, err := mounted.Egress(boundary.Name)
		if err != nil {
			return nil, err
		}
		adapter.outputs[boundary.Name] = port
	}
	return adapter, nil
}

func (adapter *fixtureSessionAdapter) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wait sync.WaitGroup
	for name, output := range adapter.outputs {
		name, output := name, output
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				envelope, err := output.Receive(ctx)
				if err != nil {
					return
				}
				switch name {
				case "observations":
					observation, ok := fixtureObservation(envelope.Payload)
					if ok {
						_ = adapter.sink.Observation(ctx, observation)
					}
				case "transcript":
					observation, ok := fixtureObservation(envelope.Payload)
					if ok {
						_ = adapter.sink.Transcript(ctx, legacy.TranscriptEvent{
							ItemID: envelope.ItemID, Text: observation.Text, Final: observation.Final,
						})
					}
				}
			}
		}()
	}
	<-ctx.Done()
	wait.Wait()
	return nil
}
func (*fixtureSessionAdapter) Update(context.Context, legacy.Settings) error { return nil }
func (adapter *fixtureSessionAdapter) Audio(ctx context.Context, frame perception.Frame) error {
	if frame.Kind != perception.FrameAudio {
		return errors.New("fixture meeting audio requires an audio frame")
	}
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	return adapter.send(ctx, "audio", frame.CapturedNS, acousticelements.InputFrame{
		StreamID: adapter.sessionID + ":microphone", Frame: frame,
	}, "microphone", "")
}
func (adapter *fixtureSessionAdapter) Video(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Kind != perception.FrameImage || frame.Source != "screen" || frame.CapturedNS == 0 {
		return errors.New("fixture meeting video requires an externally clocked screen image")
	}
	frame.Image = slices.Clone(frame.Image)
	return adapter.send(ctx, "video", frame.CapturedNS, modelelements.VideoInputFrame{
		StreamID: adapter.sessionID + ":screen", Frame: frame, FrameRateMilliHz: 5_000,
	}, "screen", adapter.sessionID+":screen")
}
func (adapter *fixtureSessionAdapter) Text(ctx context.Context, input legacy.TextInput) error {
	role := input.Role
	if role == "" {
		role = "user"
	}
	return adapter.send(ctx, "text", 0, meetinggraph.ContextInjection{
		Role: role, Source: "client.text", RunID: input.ItemID, Text: input.Text,
	}, "client.text", input.ItemID)
}
func (adapter *fixtureSessionAdapter) ToolResult(
	ctx context.Context, result trajectory.ToolResult,
) error {
	return adapter.send(ctx, "tool_result", 0, result, "client.tool", result.CallID)
}
func (adapter *fixtureSessionAdapter) CommitAudio(ctx context.Context) error {
	return adapter.send(ctx, "commit_audio", 0, acousticelements.AudioCommit{
		StreamID: adapter.sessionID + ":microphone", Reason: "client commit",
	}, "microphone", adapter.sessionID+":microphone")
}
func (adapter *fixtureSessionAdapter) CreateResponse(ctx context.Context) error {
	return adapter.send(ctx, "create_response", 0, cognitionelements.Generate{
		Invocation: continuation.Invocation{Instruction: "Respond to the current meeting context."},
	}, "client.response", "")
}
func (adapter *fixtureSessionAdapter) Cancel(ctx context.Context, reason string) error {
	runID := adapter.sessionID + ":foreground"
	return adapter.send(ctx, "cancel", 0, cognitionelements.Cancel{
		RunID: runID, Reason: reason,
	}, "client.response", runID)
}
func (adapter *fixtureSessionAdapter) Truncate(
	ctx context.Context, truncation legacy.Truncation,
) error {
	return adapter.send(ctx, "truncate", 0, truncation, "client.playback", truncation.ItemID)
}
func (*fixtureSessionAdapter) Trajectory() trajectory.Snapshot    { return trajectory.Snapshot{} }
func (*fixtureSessionAdapter) Close(context.Context, error) error { return nil }

func (adapter *fixtureSessionAdapter) send(
	ctx context.Context, boundary string, captureNS uint64, payload any, sourceID, runID string,
) error {
	if ctx == nil {
		return errors.New("fixture meeting adapter received a nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	port, found := adapter.inputs[boundary]
	if !found {
		return fmt.Errorf("fixture meeting adapter has no input %q", boundary)
	}
	sequence := adapter.sequence.Add(1)
	itemID := fmt.Sprintf("%s:adapter:%s:%d", adapter.sessionID, boundary, sequence)
	if runID == "" {
		runID = itemID
	}
	result, err := port.Broadcast(ctx, element.Envelope{
		Type: port.Type(), ItemID: itemID, SessionID: adapter.sessionID,
		SourceID: sourceID, RunID: runID, Sequence: sequence, CaptureNS: captureNS,
		TraceID: itemID, CancellationScope: runID, Payload: payload,
	})
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("fixture meeting %s delivered %d and dropped %d lanes",
			boundary, result.Delivered, result.Dropped)
	}
	return nil
}

func fixtureObservation(payload any) (perception.Observation, bool) {
	switch value := payload.(type) {
	case perception.Observation:
		value.Media = slices.Clone(value.Media)
		return value, true
	case *perception.Observation:
		if value != nil {
			result := *value
			result.Media = slices.Clone(value.Media)
			return result, true
		}
	}
	return perception.Observation{}, false
}

func fixtureCanonical(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func dependencyArtifact(name string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: "test://meeting/dependency/" + name, Revision: "fixture-1"}
}

func hasBoundary(graph ir.Graph, name string) bool {
	for _, boundary := range graph.Boundaries {
		if boundary.Name == name {
			return true
		}
	}
	return false
}

func hasEdge(graph ir.Graph, from, to string, delivery ir.Delivery) bool {
	for _, edge := range graph.Edges {
		actualFrom := edge.From.Node + "." + edge.From.Port
		actualTo := edge.To.Node + "." + edge.To.Port
		if actualFrom == from && actualTo == to && edge.Delivery == delivery {
			return true
		}
	}
	return false
}

func awaitMeetingEdgeDequeued(
	t testing.TB, mounted interface {
		Graph() ir.Graph
		Live() inspect.Live
	}, from, to string,
) inspect.EdgeLive {
	t.Helper()
	if mounted == nil {
		t.Fatal("await Meeting edge delivery with nil graph")
	}
	edgeID := ""
	for _, edge := range mounted.Graph().Edges {
		if edge.From.String() == from && edge.To.String() == to {
			edgeID = edge.ID
			break
		}
	}
	if edgeID == "" {
		t.Fatalf("Meeting graph has no edge %s -> %s", from, to)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		state := mounted.Live().Edges[edgeID]
		if state.Enqueued > 0 && state.Dequeued == state.Enqueued && state.Occupancy == 0 {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("Meeting edge %s -> %s did not drain: %+v", from, to, state)
		}
		time.Sleep(time.Millisecond)
	}
}

func writeMeetingWebSocketEvent(
	t testing.TB, connection *websocket.Conn, event map[string]any,
) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func awaitMeetingWebSocketEvent(
	t testing.TB, connection *websocket.Conn, want string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		kind, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("await meeting WebSocket event %s: %v", want, err)
		}
		if kind != websocket.MessageText {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode meeting WebSocket event: %v", err)
		}
		if event["type"] == "error" {
			t.Fatalf("meeting WebSocket error while awaiting %s: %+v", want, event)
		}
		if event["type"] == want {
			return event
		}
	}
}

func fixtureJSONStringSet(value any, want ...string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return false
		}
		seen[text] = struct{}{}
	}
	for _, expected := range want {
		if _, found := seen[expected]; !found {
			return false
		}
	}
	return true
}

func fixtureMeetingJPEG(t testing.TB, luminance uint8) []byte {
	t.Helper()
	frame := image.NewGray(image.Rect(0, 0, 32, 24))
	for y := 0; y < frame.Bounds().Dy(); y++ {
		for x := 0; x < frame.Bounds().Dx(); x++ {
			value := uint8((x*int(luminance) + y*(int(luminance)+31) + x*y*7) & 0xff)
			frame.SetGray(x, y, color.Gray{Y: value})
		}
	}
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, frame, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
}

type meetingSink struct{}

func (meetingSink) TurnBegin(context.Context) error                                   { return nil }
func (meetingSink) TurnEnd(context.Context, legacy.TurnOutcome) error                 { return nil }
func (meetingSink) Activity(context.Context, legacy.ActivityEvent) error              { return nil }
func (meetingSink) Transcript(context.Context, legacy.TranscriptEvent) error          { return nil }
func (meetingSink) Observation(context.Context, perception.Observation) error         { return nil }
func (meetingSink) SpeechBegin(context.Context, action.Utterance) error               { return nil }
func (meetingSink) SpeechText(context.Context, action.Utterance, string) error        { return nil }
func (meetingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error { return nil }
func (meetingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error { return nil }
func (meetingSink) ToolCalls(context.Context, legacy.ToolCallEvent) error             { return nil }
func (meetingSink) Failed(context.Context, legacy.ErrorEvent)                         {}

type quarantineMeetingSink struct {
	mu         sync.Mutex
	events     []string
	audio      [][]byte
	utterances []string
	done       chan struct{}
	doneOnce   sync.Once
}

func (sink *quarantineMeetingSink) record(event string) {
	sink.mu.Lock()
	sink.events = append(sink.events, event)
	sink.mu.Unlock()
}

func (sink *quarantineMeetingSink) snapshot() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return slices.Clone(sink.events)
}

func (sink *quarantineMeetingSink) audioSnapshot() ([][]byte, []string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	audio := make([][]byte, len(sink.audio))
	for index := range sink.audio {
		audio[index] = slices.Clone(sink.audio[index])
	}
	return audio, slices.Clone(sink.utterances)
}

func (sink *quarantineMeetingSink) TurnBegin(context.Context) error {
	sink.record("turn_begin")
	return nil
}
func (sink *quarantineMeetingSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	sink.record("turn_end")
	sink.doneOnce.Do(func() { close(sink.done) })
	return nil
}
func (*quarantineMeetingSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (*quarantineMeetingSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (*quarantineMeetingSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (sink *quarantineMeetingSink) SpeechBegin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	sink.events = append(sink.events, "speech_begin")
	sink.utterances = append(sink.utterances, utterance.Text)
	sink.mu.Unlock()
	return nil
}
func (sink *quarantineMeetingSink) SpeechText(
	_ context.Context, _ action.Utterance, text string,
) error {
	sink.record("text:" + text)
	return nil
}
func (sink *quarantineMeetingSink) SpeechAudio(
	_ context.Context, _ action.Utterance, frame action.Frame,
) error {
	sink.mu.Lock()
	sink.events = append(sink.events, "audio")
	sink.audio = append(sink.audio, slices.Clone(frame.PCM16LE))
	sink.mu.Unlock()
	return nil
}
func (sink *quarantineMeetingSink) SpeechEnd(
	context.Context, action.Utterance, action.Outcome,
) error {
	sink.record("speech_end")
	return nil
}
func (sink *quarantineMeetingSink) ToolCalls(_ context.Context, event legacy.ToolCallEvent) error {
	for _, call := range event.Calls {
		sink.record("tool:" + call.CallID)
	}
	return nil
}
func (*quarantineMeetingSink) Failed(context.Context, legacy.ErrorEvent) {}

var _ legacy.Sink = (*quarantineMeetingSink)(nil)
