package realtimebench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/agenttool"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/benchspec"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/livebench"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type testASR struct{}

func (*testASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "test-asr", Version: "1", Capabilities: v1.Capabilities{v1.CapabilityRevisions: true}}
}

func (*testASR) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return []v1.PerceptionRevision{{
		RevisionID: 1, SourceSample: frame.SampleOffset + uint64(len(frame.PCM16LE)/2),
		UnstableText: "lookup key",
	}}, nil
}

func (*testASR) Finalize(_ context.Context, source uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, SourceSample: source, StableText: "lookup key", Final: true}, nil
}

type testScript struct {
	events     []continuation.Event
	completion continuation.Completion
	err        error
}

type testProvider struct {
	mu         sync.Mutex
	descriptor continuation.Descriptor
	scripts    []testScript
	requests   []continuation.Request
	onContinue func(int)
}

func (provider *testProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *testProvider) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.scripts) {
		provider.mu.Unlock()
		return continuation.Completion{}, errors.New("unexpected invocation")
	}
	script := provider.scripts[index]
	onContinue := provider.onContinue
	provider.mu.Unlock()
	if onContinue != nil {
		onContinue(index)
	}
	for _, event := range script.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return script.completion, script.err
}

func (provider *testProvider) Requests() []continuation.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]continuation.Request(nil), provider.requests...)
}

type coordinatingTTS struct {
	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func (provider *coordinatingTTS) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "test-tts", Version: "1", Capabilities: v1.Capabilities{v1.CapabilityStreamingOutput: true}}
}

func (provider *coordinatingTTS) Stream(ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	provider.startedOnce.Do(func() { close(provider.started) })
	select {
	case <-provider.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return consume(v1.SpeechChunk{
		ChunkID: "audio", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
		PCM16LE: make([]byte, 480), Final: true,
	})
}

func (provider *coordinatingTTS) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

func testRegistry(t *testing.T) *agenttool.Registry {
	t.Helper()
	registry := agenttool.NewRegistry(nil)
	err := registry.Register(agenttool.Definition{
		Tool: continuation.ToolDefinition{
			Name: "lookup", Description: "Look up a test value.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`),
		},
		Capability: continuation.Capability{Name: "lookup", Description: "Look up a test value.", Available: true, ExecutionPhase: "slow"},
		ReadOnly:   true,
	}, func(context.Context, trajectory.ToolCall) (any, error) {
		return map[string]int{"value": 7}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRunComposesAudioFastSlowToolAndSpeech(t *testing.T) {
	t.Parallel()
	fast := &testProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
			NativeStateType: "test-state", RetainsToolCalls: true,
			ToolAuthority: continuation.ToolAuthorityPropose,
		},
		scripts: []testScript{{
			events:     []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I'll check."}},
			completion: continuation.Completion{ProviderStateType: "test-state", ProviderState: json.RawMessage(`{"private":"secret-native-state"}`)},
		}},
	}
	tts := &coordinatingTTS{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	slow := &testProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, Streaming: true, ExecutableTools: true,
			RetainsToolCalls: true,
		},
		scripts: []testScript{
			{events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}}}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The value is 7."}}},
		},
		onContinue: func(index int) {
			if index != 0 {
				return
			}
			select {
			case <-tts.started:
			case <-time.After(time.Second):
				t.Error("slow continuation did not overlap fast TTS")
			}
			releaseOnce.Do(func() { close(tts.release) })
		},
	}
	report, err := Run(context.Background(), Config{
		Scenario: Scenario{
			ID: "case", ReferenceText: "lookup key", ExpectedSubstrings: []string{"7"},
			RequiredToolCalls: []string{"lookup"},
			ExpectedToolCalls: []benchspec.ToolCallExpectation{{
				Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
			}},
			Audio: livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 3200)},
		},
		ASR: &testASR{}, FastProvider: fast, SlowProvider: slow, TTS: tts,
		Tools: testRegistry(t), ASRFrameDuration: 100 * time.Millisecond,
		FastMaxOutputTokens: 32, SlowMaxOutputTokens: 64,
		PrepareFastBeforeEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(report.Speech) != 2 || len(report.ModelInvocations) != 3 || len(report.ToolCalls) != 1 || len(report.ToolResults) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.FastPreparation == nil || !report.FastPreparation.Accepted ||
		report.CommittedFastInvocationID != report.FastPreparation.AcceptedInvocationID ||
		report.EndpointToFastSafePointMS == nil || *report.EndpointToFastSafePointMS != 0 {
		t.Fatalf("final-equivalent preparation was not committed: %+v", report.FastPreparation)
	}
	if report.Speech[0].Phase != trajectory.PhaseFast || report.Speech[1].Phase != trajectory.PhaseSlow {
		t.Fatalf("speech order = %+v", report.Speech)
	}
	requests := slow.Requests()
	if len(requests) != 2 || !hasQueuedAssistant(requests[0].Trajectory) {
		t.Fatalf("slow did not inherit queued fast commitment: %+v", requests)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-native-state") || strings.Contains(string(encoded), "output_pcm16") {
		t.Fatalf("private provider state or PCM leaked into report: %s", encoded)
	}
}

func TestRunPreparesFastAndSlowAsOneExactBackgroundTrajectory(t *testing.T) {
	t.Parallel()
	fast := &testProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
			ToolAuthority: continuation.ToolAuthorityPropose, RetainsToolCalls: true,
		},
		scripts: []testScript{{events: []continuation.Event{
			{Kind: continuation.EventReasoningDelta, Text: "need lookup"},
			{Kind: continuation.EventAssistantDelta, Text: "I'll check."},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
			}},
		}}},
	}
	slow := &testProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, Streaming: true,
			ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
			RetainsToolCalls: true,
		},
		scripts: []testScript{
			{events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
			}}}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The value is 7."}}},
		},
	}
	ttsRelease := make(chan struct{})
	close(ttsRelease)
	tts := &coordinatingTTS{started: make(chan struct{}), release: ttsRelease}
	report, err := Run(context.Background(), Config{
		Scenario: Scenario{
			ID: "case", ReferenceText: "lookup key", ExpectedSubstrings: []string{"7"},
			RequiredToolCalls: []string{"lookup"},
			ExpectedToolCalls: []benchspec.ToolCallExpectation{{
				Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
			}},
			Audio: livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 3200)},
		},
		ASR: &testASR{}, FastProvider: fast, FastPreparationProvider: fast,
		SlowProvider: slow, SlowPreparationProvider: slow,
		TTS: tts, Tools: testRegistry(t), ASRFrameDuration: 100 * time.Millisecond,
		FastMaxOutputTokens: 32, SlowMaxOutputTokens: 64,
		PrepareFastBeforeEndpoint: true, PrepareSlowBeforeEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.BackgroundPreparation == nil ||
		!report.BackgroundPreparation.Committed || report.BackgroundPreparation.ReplayedStages != 2 ||
		report.BackgroundPreparation.FallbackStages != 0 {
		t.Fatalf("background preparation report = %+v, passed=%v", report.BackgroundPreparation, report.Passed)
	}
	if len(report.Scoring.ExpectedToolCalls) != 1 || string(report.Scoring.ExpectedToolCalls[0].Arguments) != `{"key":"x"}` {
		t.Fatalf("report omitted exact scoring contract: %+v", report.Scoring)
	}
	if report.Scoring.Version != benchspec.ToolTrajectoryScorerVersion {
		t.Fatalf("scorer version = %q", report.Scoring.Version)
	}
	if report.FastPreparation != nil {
		t.Fatalf("legacy fast-only preparation unexpectedly ran: %+v", report.FastPreparation)
	}
	if len(report.ToolProposals) != 1 || len(report.ToolCalls) != 1 || len(report.ToolResults) != 1 {
		t.Fatalf("tool trajectory: proposals=%+v calls=%+v results=%+v", report.ToolProposals, report.ToolCalls, report.ToolResults)
	}
	if report.ToolProposals[0].CallID == report.ToolCalls[0].CallID {
		t.Fatal("proposal and executable call identities collided")
	}
	if len(report.ModelInvocations) != 3 {
		t.Fatalf("model invocation count = %d, want prepared fast + prepared slow + live slow-after-result", len(report.ModelInvocations))
	}
	requests := slow.Requests()
	if len(requests) != 2 || !containsTrajectoryKind(requests[0].Trajectory, trajectory.KindReasoning) ||
		!containsTrajectoryKind(requests[0].Trajectory, trajectory.KindAssistant) ||
		!containsTrajectoryKind(requests[0].Trajectory, trajectory.KindToolProposal) {
		t.Fatalf("slow requests did not inherit prepared fast trajectory: %+v", requests)
	}
}

func TestWriteReportPublishesAtomically(t *testing.T) {
	t.Parallel()
	filename := filepath.Join(t.TempDir(), "nested", "report.json")
	if err := WriteReport(filename, Report{ScenarioID: "case"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != SchemaVersion || report.CreatedAt.IsZero() || report.ScenarioID != "case" {
		t.Fatalf("report = %+v", report)
	}
}

func hasQueuedAssistant(snapshot trajectory.Snapshot) bool {
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindAssistantState && item.AssistantState != nil && item.AssistantState.Visibility == trajectory.VisibilityQueued {
			return true
		}
	}
	return false
}

func containsTrajectoryKind(snapshot trajectory.Snapshot, kind trajectory.Kind) bool {
	for _, item := range snapshot.Items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

var _ v1.PerceptionProvider = (*testASR)(nil)
var _ continuation.Provider = (*testProvider)(nil)
var _ v1.StreamingSpeechProvider = (*coordinatingTTS)(nil)
