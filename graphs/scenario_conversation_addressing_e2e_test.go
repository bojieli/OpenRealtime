package graphs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphinspect "github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	scenarioAddressingSession = "scenario-addressing-session"
	scenarioAddressingPolicy  = "do whatever Tim asks about the printer"
)

// TestScenarioConversationFinalNamedThirdPartyRequestNeverAcquiresAgentAuthority
// reconstructs the talking_to_other/91 lifecycle at the mounted production
// graph boundary. The first answer is fully played before the named addressee
// starts, so this is deliberately not an overlap-timing test: the final ASR
// observation must still be screened before it can invoke cognition, speech,
// or standing-policy mutation.
func TestScenarioConversationFinalNamedThirdPartyRequestNeverAcquiresAgentAuthority(t *testing.T) {
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	asr := &scenarioAddressingASRControl{turns: []string{
		"Translate good morning into Spanish.",
		"Tim, printer's jammed again-help?",
		"Actually, what are some financial goals I should set?",
	}}
	policy := newScenarioAddressingPolicyControl()
	models := newScenarioAddressingModelControl()
	tts := &scenarioAddressingTTSControl{}

	config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
		return &scenarioAddressingASR{control: asr, descriptor: config.ASR.Descriptor}, nil
	}
	config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		return &scenarioAddressingPolicyDecider{control: policy, descriptor: config.Policy.Descriptor}, nil
	}
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return &scenarioAddressingModel{control: models, descriptor: config.Model.Descriptor}, nil
	}
	config.SilentModel.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return &scenarioAddressingModel{control: models, descriptor: config.SilentModel.Descriptor}, nil
	}
	config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
		return &scenarioAddressingTTS{control: tts, descriptor: config.TTS.Descriptor}, nil
	}

	launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	recording := newScenarioAddressingGraphRecorder()
	instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording,
		"decision", "outcome", "voice_committed", "silent_committed")
	instrumentScenarioAddressingFactory(t, &launchConfig, "interaction.OverlapBargeIn", recording,
		"model_cancel", "segmentation_cancel", "tts_cancel", "playback_cancel")
	launched, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}

	sink := newScenarioAddressingSink()
	settings := legacy.Settings{
		Instruction: "Answer requests addressed to this assistant and never join another person's conversation.",
		Voice:       config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
	}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		SessionID: scenarioAddressingSession, Sink: sink, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("addressing lifecycle test complete")); closeErr != nil {
			t.Errorf("close scenario addressing runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	var audioClock scenarioAddressingAudioClock
	firstStream := scenarioAddressingStreamID(scenarioAddressingSession, 1)
	driveScenarioAddressingTurn(t, runtime, sink, &audioClock, firstStream, asr.turns[0])
	receiveScenarioAddressing(t, sink.speechEnded, "completed initial response")
	firstOutcome := recording.await(t, "semantic_admission.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(policyelements.SemanticAdmissionOutcome)
		return ok && outcome.StreamID == firstStream
	})
	assertScenarioAddressingAdmission(t, firstOutcome, firstStream, 1,
		policyelements.SemanticAdmissionAdmitted, "")
	assertScenarioAddressingCommitBranch(t, recording, "voice_committed", firstStream, 1, true)
	first := scenarioAddressingSnapshot(t, runtime, sink, models, tts, recording)
	if first.models != 1 || first.ttsPlans != 1 || first.speechEnds != 1 {
		t.Fatalf("initial direct turn did not become one completed response: %+v", first)
	}

	// The named request begins only after SpeechEnd, matching /91's defining
	// condition: there is no model, segmentation, TTS, or playback work left
	// for the overlap policy to cancel.
	secondStream := scenarioAddressingStreamID(scenarioAddressingSession, 2)
	driveScenarioAddressingTurn(t, runtime, sink, &audioClock, secondStream, asr.turns[1])
	secondOutcome := recording.await(t, "semantic_admission.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(policyelements.SemanticAdmissionOutcome)
		return ok && outcome.StreamID == secondStream
	})
	assertScenarioAddressingAdmission(t, secondOutcome, secondStream, 2,
		policyelements.SemanticAdmissionSuppressed, "addressed_elsewhere")
	secondDecision := recording.await(t, "semantic_admission.decision", func(envelope element.Envelope) bool {
		decision, ok := envelope.Payload.(policyelements.SemanticDecision)
		return ok && decision.StreamID == secondStream
	}).Payload.(policyelements.SemanticDecision)
	if secondDecision.SourceRevision != 2 || secondDecision.Act != coreinteraction.ActStaySilent ||
		secondDecision.DecisionStage != "voice_addressing" ||
		secondDecision.Activation != "addressed-elsewhere" || secondDecision.StandingBefore != 0 ||
		secondDecision.StandingAfter != 0 || secondDecision.StandingPinned != 0 ||
		secondDecision.StandingRevoked != 0 {
		t.Fatalf("named third-party semantic decision = %+v", secondDecision)
	}
	assertScenarioAddressingCommitBranch(t, recording, "voice_committed", secondStream, 2, false)
	assertScenarioAddressingCommitBranch(t, recording, "silent_committed", secondStream, 2, false)
	second := scenarioAddressingSnapshot(t, runtime, sink, models, tts, recording)
	if second.models != first.models || second.ttsPlans != first.ttsPlans ||
		second.speechBegins != first.speechBegins || second.speechEnds != first.speechEnds ||
		second.modelCancels != first.modelCancels || second.segmentationCancels != first.segmentationCancels ||
		second.ttsCancels != first.ttsCancels || second.playbackCancels != first.playbackCancels {
		t.Fatalf("named third-party turn acquired data-plane or cancellation authority: before=%+v after=%+v",
			first, second)
	}
	if policy.extracted(asr.turns[1]) {
		t.Fatal("named third-party speech reached standing-policy extraction")
	}
	assertScenarioAddressingTrajectoryObservation(t, runtime.Trajectory(), asr.turns[1], 2)

	thirdStream := scenarioAddressingStreamID(scenarioAddressingSession, 3)
	driveScenarioAddressingTurn(t, runtime, sink, &audioClock, thirdStream, asr.turns[2])
	thirdOutcome := recording.await(t, "semantic_admission.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(policyelements.SemanticAdmissionOutcome)
		return ok && outcome.StreamID == thirdStream
	})
	assertScenarioAddressingAdmission(t, thirdOutcome, thirdStream, 3,
		policyelements.SemanticAdmissionAdmitted, "")
	assertScenarioAddressingCommitBranch(t, recording, "voice_committed", thirdStream, 3, true)
	receiveScenarioAddressing(t, sink.speechEnded, "response to later direct request")
	thirdDecision := recording.await(t, "semantic_admission.decision", func(envelope element.Envelope) bool {
		decision, ok := envelope.Payload.(policyelements.SemanticDecision)
		return ok && decision.StreamID == thirdStream
	}).Payload.(policyelements.SemanticDecision)
	if thirdDecision.SourceRevision != 3 || thirdDecision.Act != coreinteraction.ActAnswer ||
		thirdDecision.StandingBefore != 0 || thirdDecision.StandingAfter != 0 ||
		thirdDecision.StandingPinned != 0 || thirdDecision.StandingRevoked != 0 {
		t.Fatalf("later direct-request semantic decision = %+v", thirdDecision)
	}
	third := scenarioAddressingSnapshot(t, runtime, sink, models, tts, recording)
	if third.models != first.models+1 || third.ttsPlans != first.ttsPlans+1 ||
		third.speechBegins != first.speechBegins+1 || third.speechEnds != first.speechEnds+1 ||
		third.modelCancels != first.modelCancels || third.segmentationCancels != first.segmentationCancels ||
		third.ttsCancels != first.ttsCancels || third.playbackCancels != first.playbackCancels {
		t.Fatalf("later direct request was not independently admitted: first=%+v third=%+v", first, third)
	}
	if evidence := policy.decisionEvidenceFor(asr.turns[2]); strings.Contains(evidence, scenarioAddressingPolicy) {
		t.Fatalf("named third party mutated standing-policy memory visible to the next stream: %q", evidence)
	}
	assertScenarioAddressingTrajectoryObservation(t, runtime.Trajectory(), asr.turns[2], 3)

	for _, node := range []string{"voice_model", "segment", "tts", "playback"} {
		if before, after := first.nodeCancellation[node], third.nodeCancellation[node]; before != after {
			t.Fatalf("%s cancellation timestamp changed across non-overlapping turns: %d -> %d", node, before, after)
		}
	}
}

type scenarioAddressingASRControl struct {
	mu       sync.Mutex
	turns    []string
	revision uint64
	finals   int
}

type scenarioAddressingASR struct {
	control    *scenarioAddressingASRControl
	descriptor v1.Descriptor
}

func (provider *scenarioAddressingASR) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *scenarioAddressingASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	provider.control.mu.Lock()
	defer provider.control.mu.Unlock()
	if provider.control.finals >= len(provider.control.turns) {
		return nil, errors.New("scenario addressing ASR received audio after its script ended")
	}
	provider.control.revision++
	text := provider.control.turns[provider.control.finals]
	return []v1.PerceptionRevision{{
		RevisionID: provider.control.revision, SourceSample: frame.SampleOffset,
		StableText: text,
	}}, nil
}

func (provider *scenarioAddressingASR) Finalize(
	_ context.Context, sample uint64,
) (v1.PerceptionRevision, error) {
	provider.control.mu.Lock()
	defer provider.control.mu.Unlock()
	if provider.control.finals >= len(provider.control.turns) {
		return v1.PerceptionRevision{}, errors.New("scenario addressing ASR finalized after its script ended")
	}
	provider.control.revision++
	text := provider.control.turns[provider.control.finals]
	provider.control.finals++
	return v1.PerceptionRevision{
		RevisionID: provider.control.revision, SourceSample: sample,
		StableText: text, Final: true,
	}, nil
}

type scenarioAddressingPolicyRecord struct {
	options  []string
	evidence string
	answer   string
}

type scenarioAddressingGenerationRecord struct {
	prompt   string
	evidence string
}

type scenarioAddressingPolicyControl struct {
	mu          sync.Mutex
	decisions   []scenarioAddressingPolicyRecord
	generations []scenarioAddressingGenerationRecord
}

func newScenarioAddressingPolicyControl() *scenarioAddressingPolicyControl {
	return &scenarioAddressingPolicyControl{}
}

type scenarioAddressingPolicyDecider struct {
	control    *scenarioAddressingPolicyControl
	descriptor policyelements.SemanticDeciderDescriptor
}

func (*scenarioAddressingPolicyDecider) Name() string { return "scenario-addressing-policy" }
func (decider *scenarioAddressingPolicyDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *scenarioAddressingPolicyDecider) Decide(
	_ context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	currentEvidence := scenarioAddressingCurrentEvidence(decision.Evidence)
	wanted := ""
	switch {
	case slices.Contains(decision.Options, "addressed-elsewhere"):
		wanted = "direct-request"
		if strings.Contains(currentEvidence, "Tim, printer's jammed again-help?") {
			wanted = "addressed-elsewhere"
		}
	case slices.Contains(decision.Options, string(coreinteraction.OverlapDirected)):
		wanted = string(coreinteraction.OverlapDirected)
		if strings.Contains(currentEvidence, "Tim, printer's jammed again-help?") {
			wanted = string(coreinteraction.OverlapSide)
		}
	case slices.Contains(decision.Options, string(coreinteraction.ActAnswer)):
		wanted = string(coreinteraction.ActAnswer)
	case slices.Contains(decision.Options, "covered"):
		wanted = "covered"
	case slices.Contains(decision.Options, "action-ready"):
		wanted = "wait"
	}
	index := slices.Index(decision.Options, wanted)
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf(
			"scenario addressing policy has no deterministic answer for options %v", decision.Options,
		)
	}
	decider.control.mu.Lock()
	decider.control.decisions = append(decider.control.decisions, scenarioAddressingPolicyRecord{
		options: slices.Clone(decision.Options), evidence: decision.Evidence, answer: wanted,
	})
	decider.control.mu.Unlock()
	return coreinteraction.Outcome{Index: index, Option: wanted, Confidence: 0.99, Measured: true}, nil
}

func scenarioAddressingCurrentEvidence(evidence string) string {
	_, current, found := strings.Cut(evidence, "\nNow:\n")
	if !found {
		return evidence
	}
	for _, line := range strings.Split(current, "\n") {
		if strings.HasPrefix(line, "heard from ") && strings.Contains(line, " so far: ") {
			return line
		}
	}
	return current
}

func (decider *scenarioAddressingPolicyDecider) Generate(
	_ context.Context, prompt, evidence string, _ int,
) (string, error) {
	decider.control.mu.Lock()
	decider.control.generations = append(decider.control.generations,
		scenarioAddressingGenerationRecord{prompt: prompt, evidence: evidence})
	decider.control.mu.Unlock()
	switch prompt {
	case coreinteraction.ExtractionInstruction:
		if strings.Contains(evidence, `They just said: "Tim, printer's jammed again-help?"`) {
			// This intentionally hostile extractor answer proves that addressing is
			// screened before policy mutation, not merely before model invocation.
			return "pin conversation " + scenarioAddressingPolicy, nil
		}
		return "none", nil
	case coreinteraction.StandingPolicyGroundingInstruction:
		return "yes", nil
	case coreinteraction.CountingInstruction, coreinteraction.RestrictingInstruction:
		return "no", nil
	case coreinteraction.ScopeInstruction:
		return "standing", nil
	default:
		return "none", nil
	}
}

func (control *scenarioAddressingPolicyControl) extracted(text string) bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	for _, record := range control.generations {
		if record.prompt == coreinteraction.ExtractionInstruction && strings.Contains(record.evidence, text) {
			return true
		}
	}
	return false
}

func (control *scenarioAddressingPolicyControl) decisionEvidenceFor(text string) string {
	control.mu.Lock()
	defer control.mu.Unlock()
	for index := len(control.decisions) - 1; index >= 0; index-- {
		if strings.Contains(control.decisions[index].evidence, text) {
			return control.decisions[index].evidence
		}
	}
	return ""
}

type scenarioAddressingModelControl struct {
	invocations atomic.Int32
	mu          sync.Mutex
	requests    []continuation.Request
}

func newScenarioAddressingModelControl() *scenarioAddressingModelControl {
	return &scenarioAddressingModelControl{}
}

type scenarioAddressingModel struct {
	control    *scenarioAddressingModelControl
	descriptor continuation.Descriptor
}

func (provider *scenarioAddressingModel) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *scenarioAddressingModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.control.mu.Lock()
	provider.control.requests = append(provider.control.requests, request)
	provider.control.mu.Unlock()
	provider.control.invocations.Add(1)
	latest := ""
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindObservation && item.Producer.Phase == trajectory.PhaseUser {
			latest = item.Content
		}
	}
	if strings.Contains(latest, "Tim, printer's jammed") {
		return continuation.Completion{}, errors.New("named third-party speech reached cognition")
	}
	return scenarioEndpointSpeak(emit, "Response to the request addressed to this assistant.")
}

type scenarioAddressingTTSControl struct{ plans atomic.Int32 }

type scenarioAddressingTTS struct {
	control    *scenarioAddressingTTSControl
	descriptor v1.Descriptor
}

func (provider *scenarioAddressingTTS) Descriptor() v1.Descriptor { return provider.descriptor }
func (provider *scenarioAddressingTTS) Synthesize(
	ctx context.Context, plan v1.SpeechPlan,
) ([]v1.SpeechChunk, error) {
	var result []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		result = append(result, chunk)
		return nil
	})
	return result, err
}
func (provider *scenarioAddressingTTS) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	provider.control.plans.Add(1)
	return emit(v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":addressing-audio", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: scenarioEndpointPCM(2_400), Final: true,
	})
}

type scenarioAddressingSink struct {
	mu           sync.Mutex
	turnBegins   int
	turnEnds     int
	speechBegins int
	speechTexts  int
	speechAudio  int
	speechEnds   int
	failures     []legacy.ErrorEvent
	activities   chan legacy.ActivityEvent
	transcripts  chan legacy.TranscriptEvent
	speechEnded  chan legacyaction.Outcome
}

func newScenarioAddressingSink() *scenarioAddressingSink {
	return &scenarioAddressingSink{
		activities:  make(chan legacy.ActivityEvent, 16),
		transcripts: make(chan legacy.TranscriptEvent, 64),
		speechEnded: make(chan legacyaction.Outcome, 8),
	}
}

func (sink *scenarioAddressingSink) TurnBegin(context.Context) error {
	sink.mu.Lock()
	sink.turnBegins++
	sink.mu.Unlock()
	return nil
}
func (sink *scenarioAddressingSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	sink.mu.Lock()
	sink.turnEnds++
	sink.mu.Unlock()
	return nil
}
func (sink *scenarioAddressingSink) Activity(_ context.Context, event legacy.ActivityEvent) error {
	sink.activities <- event
	return nil
}
func (sink *scenarioAddressingSink) Transcript(_ context.Context, event legacy.TranscriptEvent) error {
	sink.transcripts <- event
	return nil
}
func (*scenarioAddressingSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *scenarioAddressingSink) SpeechBegin(context.Context, legacyaction.Utterance) error {
	sink.mu.Lock()
	sink.speechBegins++
	sink.mu.Unlock()
	return nil
}
func (sink *scenarioAddressingSink) SpeechText(
	context.Context, legacyaction.Utterance, string,
) error {
	sink.mu.Lock()
	sink.speechTexts++
	sink.mu.Unlock()
	return nil
}
func (sink *scenarioAddressingSink) SpeechAudio(
	context.Context, legacyaction.Utterance, legacyaction.Frame,
) error {
	sink.mu.Lock()
	sink.speechAudio++
	sink.mu.Unlock()
	return nil
}
func (sink *scenarioAddressingSink) SpeechEnd(
	_ context.Context, _ legacyaction.Utterance, outcome legacyaction.Outcome,
) error {
	sink.mu.Lock()
	sink.speechEnds++
	sink.mu.Unlock()
	sink.speechEnded <- outcome
	return nil
}
func (*scenarioAddressingSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (sink *scenarioAddressingSink) Failed(_ context.Context, failure legacy.ErrorEvent) {
	sink.mu.Lock()
	sink.failures = append(sink.failures, failure)
	sink.mu.Unlock()
}

type scenarioAddressingAudioClock struct {
	index  uint64
	sample uint64
	nowNS  uint64
}

func driveScenarioAddressingTurn(
	t *testing.T, runtime legacy.Runtime, sink *scenarioAddressingSink,
	clock *scenarioAddressingAudioClock, streamID, finalText string,
) {
	t.Helper()
	for index := 0; index < 8; index++ {
		clock.index++
		clock.nowNS += uint64(100 * time.Millisecond)
		pcm := scenarioEndpointPCM(2_400)
		if index >= 3 {
			pcm = make([]byte, 4_800)
		}
		frame := perception.Frame{
			Kind: perception.FrameAudio, Source: scenarioconversation.SourceMicrophone,
			CapturedNS: clock.nowNS, Index: clock.index, PCM16LE: pcm,
			SampleRateHz: 24_000, SampleOffset: clock.sample,
		}
		clock.sample += 2_400
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runtime.Audio(ctx, frame)
		cancel()
		if err != nil {
			t.Fatalf("send addressing audio frame %d for %s: %v", index, streamID, err)
		}
	}
	var started, stopped bool
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !started || !stopped {
		select {
		case event := <-sink.activities:
			if event.ItemID != streamID {
				t.Fatalf("audio activity crossed streams: got %+v, want %q", event, streamID)
			}
			started = started || event.Started
			stopped = stopped || event.Stopped
		case <-deadline.C:
			t.Fatalf("timed out waiting for exact activity lifecycle on %s", streamID)
		}
	}
	transcriptDeadline := time.NewTimer(5 * time.Second)
	defer transcriptDeadline.Stop()
	for {
		select {
		case event := <-sink.transcripts:
			if event.Final {
				if event.Text != finalText {
					t.Fatalf("final transcript on %s = %q, want %q", streamID, event.Text, finalText)
				}
				return
			}
		case <-transcriptDeadline.C:
			t.Fatalf("timed out waiting for final transcript on %s", streamID)
		}
	}
}

type scenarioAddressingRuntimeSnapshot struct {
	models, ttsPlans, turnBegins, turnEnds int
	speechBegins, speechTexts              int
	speechAudio, speechEnds                int
	modelCancels, segmentationCancels      int
	ttsCancels, playbackCancels            int
	nodeCancellation                       map[string]uint64
}

func scenarioAddressingSnapshot(
	t *testing.T,
	runtime legacy.Runtime, sink *scenarioAddressingSink,
	models *scenarioAddressingModelControl, tts *scenarioAddressingTTSControl,
	recording *scenarioAddressingGraphRecorder,
) scenarioAddressingRuntimeSnapshot {
	t.Helper()
	sink.mu.Lock()
	snapshot := scenarioAddressingRuntimeSnapshot{
		models: int(models.invocations.Load()), ttsPlans: int(tts.plans.Load()),
		turnBegins: sink.turnBegins, turnEnds: sink.turnEnds,
		speechBegins: sink.speechBegins, speechTexts: sink.speechTexts,
		speechAudio: sink.speechAudio, speechEnds: sink.speechEnds,
		modelCancels:        len(recording.records("overlap_barge_in.model_cancel")),
		segmentationCancels: len(recording.records("overlap_barge_in.segmentation_cancel")),
		ttsCancels:          len(recording.records("overlap_barge_in.tts_cancel")),
		playbackCancels:     len(recording.records("overlap_barge_in.playback_cancel")),
		nodeCancellation:    make(map[string]uint64, 4),
	}
	sink.mu.Unlock()
	// NativeRuntime exposes typed live inspection in addition to the stable
	// legacy.Runtime surface. Keep the assertion local so the behavioral test
	// does not reach into graph implementation fields.
	liveRuntime, ok := runtime.(interface{ Live() graphinspect.Live })
	if !ok {
		t.Fatalf("scenario runtime %T does not expose live graph inspection", runtime)
	}
	live := liveRuntime.Live()
	for _, node := range []string{"voice_model", "segment", "tts", "playback"} {
		nodeLive, found := live.Nodes[node]
		if !found {
			t.Fatalf("live scenario graph omitted node %q", node)
		}
		snapshot.nodeCancellation[node] = nodeLive.CancellationNS
	}
	return snapshot
}

func assertScenarioAddressingAdmission(
	t *testing.T, envelope element.Envelope, streamID string, sourceRevision uint64,
	kind policyelements.SemanticAdmissionOutcomeKind, code string,
) {
	t.Helper()
	outcome, ok := envelope.Payload.(policyelements.SemanticAdmissionOutcome)
	if !ok || outcome.Kind != kind || outcome.Operation != "committed" ||
		outcome.StreamID != streamID || outcome.SourceRevision != sourceRevision || outcome.Code != code {
		t.Fatalf("semantic admission outcome = %+v (%T), want %s stream=%s revision=%d code=%q",
			envelope.Payload, envelope.Payload, kind, streamID, sourceRevision, code)
	}
}

func assertScenarioAddressingCommitBranch(
	t *testing.T, recording *scenarioAddressingGraphRecorder, port, streamID string,
	sourceRevision uint64, want bool,
) {
	t.Helper()
	got := recording.any("semantic_admission."+port, func(envelope element.Envelope) bool {
		commit, ok := envelope.Payload.(stateelements.ObservationCommitOutcome)
		return ok && commit.StreamID == streamID && commit.SourceRevision == sourceRevision
	})
	if got != want {
		t.Fatalf("semantic admission %s branch for stream=%s revision=%d present=%t, want %t",
			port, streamID, sourceRevision, got, want)
	}
}

func assertScenarioAddressingTrajectoryObservation(
	t *testing.T, snapshot trajectory.Snapshot, text string, sourceRevision uint64,
) {
	t.Helper()
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindObservation || item.Content != text {
			continue
		}
		if item.SourceRevision != sourceRevision || item.Producer.Phase != trajectory.PhaseUser ||
			item.Event == nil || item.Event.Channel != scenarioconversation.SourceMicrophone ||
			item.Event.CorrelationID == "" || !strings.HasSuffix(item.Event.Type, ".endpoint") {
			t.Fatalf("trajectory observation for %q = %+v event=%+v", text, item, item.Event)
		}
		return
	}
	t.Fatalf("trajectory omitted final microphone observation %q", text)
}

type scenarioAddressingGraphRecorder struct {
	mu      sync.Mutex
	outputs map[string][]element.Envelope
	changed chan struct{}
}

func newScenarioAddressingGraphRecorder() *scenarioAddressingGraphRecorder {
	return &scenarioAddressingGraphRecorder{
		outputs: make(map[string][]element.Envelope), changed: make(chan struct{}, 1),
	}
}

func (recorder *scenarioAddressingGraphRecorder) record(key string, envelope element.Envelope) {
	recorder.mu.Lock()
	recorder.outputs[key] = append(recorder.outputs[key], envelope.Clone())
	recorder.mu.Unlock()
	select {
	case recorder.changed <- struct{}{}:
	default:
	}
}

func (recorder *scenarioAddressingGraphRecorder) records(key string) []element.Envelope {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([]element.Envelope, len(recorder.outputs[key]))
	for index := range recorder.outputs[key] {
		result[index] = recorder.outputs[key][index].Clone()
	}
	return result
}

func (recorder *scenarioAddressingGraphRecorder) any(
	key string, predicate func(element.Envelope) bool,
) bool {
	for _, envelope := range recorder.records(key) {
		if predicate(envelope) {
			return true
		}
	}
	return false
}

func (recorder *scenarioAddressingGraphRecorder) await(
	t *testing.T, key string, predicate func(element.Envelope) bool,
) element.Envelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		for _, envelope := range recorder.records(key) {
			if predicate(envelope) {
				return envelope
			}
		}
		select {
		case <-recorder.changed:
		case <-timer.C:
			t.Fatalf("timed out waiting for mounted graph output %s", key)
			return element.Envelope{}
		}
	}
}

type scenarioAddressingRecordingFactory struct {
	base     element.Factory
	recorder *scenarioAddressingGraphRecorder
	ports    map[string]struct{}
}

func (factory scenarioAddressingRecordingFactory) Descriptor() element.Descriptor {
	return factory.base.Descriptor()
}

func (factory scenarioAddressingRecordingFactory) ValidateConfig(source json.RawMessage) error {
	validator, ok := factory.base.(element.ConfigValidator)
	if !ok {
		return nil
	}
	return validator.ValidateConfig(source)
}

func (factory scenarioAddressingRecordingFactory) Mount(
	ctx context.Context, mount element.MountContext,
) (element.Runnable, error) {
	mount.Ports = scenarioAddressingRecordingPorts{
		Ports: mount.Ports, instance: mount.InstanceID,
		recorder: factory.recorder, selected: factory.ports,
	}
	return factory.base.Mount(ctx, mount)
}

type scenarioAddressingRecordingPorts struct {
	element.Ports
	instance string
	recorder *scenarioAddressingGraphRecorder
	selected map[string]struct{}
}

func (ports scenarioAddressingRecordingPorts) Output(name string) (element.OutputPort, error) {
	output, err := ports.Ports.Output(name)
	if err != nil {
		return nil, err
	}
	if _, selected := ports.selected[name]; !selected {
		return output, nil
	}
	return scenarioAddressingRecordingOutput{
		OutputPort: output, key: ports.instance + "." + name, recorder: ports.recorder,
	}, nil
}

type scenarioAddressingRecordingOutput struct {
	element.OutputPort
	key      string
	recorder *scenarioAddressingGraphRecorder
}

func (output scenarioAddressingRecordingOutput) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	delivery, err := output.OutputPort.Broadcast(ctx, envelope)
	if err == nil && delivery.Delivered > 0 {
		output.recorder.record(output.key, envelope)
	}
	return delivery, err
}

func instrumentScenarioAddressingFactory(
	t *testing.T, config *graphlaunch.Config, elementName string,
	recorder *scenarioAddressingGraphRecorder, ports ...string,
) {
	t.Helper()
	selected := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		selected[port] = struct{}{}
	}
	for index := range config.Catalog.Assembly.Implementations {
		registration := &config.Catalog.Assembly.Implementations[index]
		if registration.Factory.Descriptor().Name != elementName {
			continue
		}
		registration.Factory = scenarioAddressingRecordingFactory{
			base: registration.Factory, recorder: recorder, ports: selected,
		}
		return
	}
	t.Fatalf("scenario graph implementation inventory omitted %s", elementName)
}

func scenarioAddressingStreamID(sessionID string, revision uint64) string {
	// Match the adapter's explicit length-prefix derivation without depending
	// on an unexported helper. This assertion protects the public stream
	// address, not an incidental random ID.
	digest := sha256.New()
	for _, value := range []string{sessionID, fmt.Sprint(revision)} {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = digest.Write([]byte(value))
	}
	return "audio:sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func receiveScenarioAddressing[T any](t *testing.T, source <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-source:
		return value
	case <-time.After(5 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", label)
		return zero
	}
}

var (
	_ v1.PerceptionProvider          = (*scenarioAddressingASR)(nil)
	_ policyelements.SemanticDecider = (*scenarioAddressingPolicyDecider)(nil)
	_ continuation.Provider          = (*scenarioAddressingModel)(nil)
	_ v1.StreamingSpeechProvider     = (*scenarioAddressingTTS)(nil)
	_ legacy.Sink                    = (*scenarioAddressingSink)(nil)
	_ element.Factory                = scenarioAddressingRecordingFactory{}
	_ element.ConfigValidator        = scenarioAddressingRecordingFactory{}
)
