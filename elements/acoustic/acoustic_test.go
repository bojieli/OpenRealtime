package acoustic

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/flow"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

func TestComponentGraphCompilesWithTypedVisibleControlCycle(t *testing.T) {
	source := readComponentFile(t, "agent.ortg")
	file, err := syntax.Parse("agent.ortg", source)
	if err != nil {
		t.Fatal(err)
	}
	catalog := acousticCatalog(t)
	lock, err := resolve.ParseLock(readComponentFile(t, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesDocument, err := graphvalues.ParseYAML("agent.values.yaml", readComponentFile(t, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, valuesDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.Graph.Validate(); err != nil {
		t.Fatal(err)
	}
	wantBoundaries := map[string]string{
		"audio": "Stream<audio.InputFrame>", "tick": "Trigger<timing.Tick>",
		"commit": "Trigger<audio.Commit>", "verdict": "Trigger<acoustic.EndpointVerdict>",
		"cancel": "Interrupt<audio.StreamID>", "admitted": "Trigger<audio.FrameBatch>",
		"flush": "Trigger<audio.Flush>", "activity": "Stream<acoustic.SpeechActivity>",
		"candidate": "Trigger<acoustic.EndpointCandidate>", "command": "Trigger<acoustic.GateCommand>",
		"admission_state":   "State<acoustic.AdmissionState>",
		"policy_state":      "State<acoustic.EndpointPolicyState>",
		"admission_outcome": "Event<acoustic.AdmissionOutcome>",
		"endpoint_outcome":  "Event<acoustic.EndpointOutcome>",
		"cancel_downstream": "Interrupt<audio.StreamID>",
	}
	if len(bound.Graph.Boundaries) != len(wantBoundaries) {
		t.Fatalf("boundary count = %d, want %d", len(bound.Graph.Boundaries), len(wantBoundaries))
	}
	for _, boundary := range bound.Graph.Boundaries {
		want, found := wantBoundaries[boundary.Name]
		if !found {
			t.Fatalf("unexpected boundary %q", boundary.Name)
		}
		if got := boundary.Type.String(); got != want {
			t.Fatalf("boundary %s type = %s, want %s", boundary.Name, got, want)
		}
	}
	admission := findNode(t, bound.Graph, "admission")
	endpoint := findNode(t, bound.Graph, "endpoint_policy")
	if !admission.Reaction.BreaksCycles || !endpoint.Reaction.BreaksCycles {
		t.Fatal("both stateful halves must explicitly break the endpoint feedback cycle")
	}
}

func TestEnergyAdmissionRejectsTransientAndPreservesPrefixAtSustainedOnset(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})

	harness.send("audio", "noise-1", input("stream-noise", audioFrame("microphone", 1, 2_000)))
	harness.send("audio", "noise-2", input("stream-noise", audioFrame("microphone", 2, 0)))
	assertNoEnvelope(t, harness.output("activity"), 75*time.Millisecond)
	assertNoEnvelope(t, harness.output("admitted"), 75*time.Millisecond)
	assertNoEnvelope(t, harness.output("candidate"), 75*time.Millisecond)

	harness.send("cancel", "cancel-noise", perceptionelements.Cancel{StreamID: "stream-noise"})
	_ = receive(t, harness.output("cancel_downstream"))
	awaitAdmissionOutcome(t, harness, "cancel-noise", "canceled")
	awaitEndpointOutcome(t, harness, "cancel-noise", "canceled")

	// One quiet prefix frame followed by two voiced frames reaches the admitted
	// batch intact when the 40 ms onset hysteresis opens.
	harness.send("audio", "speech-prefix", input("stream-speech", audioFrame("microphone", 3, 0)))
	harness.send("audio", "speech-onset-1", input("stream-speech", audioFrame("microphone", 4, 2_000)))
	harness.send("audio", "speech-onset-2", input("stream-speech", audioFrame("microphone", 5, 2_000)))
	started := payload[SpeechActivity](t, receive(t, harness.output("activity")))
	if started.Kind != SpeechStarted || started.StreamID != "stream-speech" {
		t.Fatalf("started activity = %+v", started)
	}
	batch := payload[perceptionelements.AudioBatch](t, receive(t, harness.output("admitted")))
	if len(batch.Frames) != 1 || len(batch.Frames[0].PCM16LE) != 3*audioFrameBytes {
		t.Fatalf("prefix-preserving batch = %d frames/%d bytes", len(batch.Frames), len(batch.Frames[0].PCM16LE))
	}
}

func TestSilenceIsCandidateBeforeAutomaticClose(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})
	openSpeech(t, harness, "automatic")
	harness.send("audio", "auto-silence-1", input("automatic", audioFrame("microphone", 3, 0)))
	harness.send("audio", "auto-silence-2", input("automatic", audioFrame("microphone", 4, 0)))

	candidateEnvelope := receive(t, harness.output("candidate"))
	candidate := payload[EndpointCandidate](t, candidateEnvelope)
	if candidate.StreamID != "automatic" || candidate.ID == "" || candidate.SilenceNS == 0 {
		t.Fatalf("candidate = %+v", candidate)
	}
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.Action != GateClose || command.CandidateID != candidate.ID {
		t.Fatalf("automatic command = %+v, candidate = %+v", command, candidate)
	}
	stopped := payload[SpeechActivity](t, receive(t, harness.output("activity")))
	if stopped.Kind != SpeechStopped || stopped.StreamID != "automatic" {
		t.Fatalf("stopped activity = %+v", stopped)
	}
	flushEnvelope := receive(t, harness.output("flush"))
	flush := payload[perceptionelements.Flush](t, flushEnvelope)
	if flush.StreamID != "automatic" || flush.AfterItemID == "" ||
		!slices.Contains(flushEnvelope.CausalParents, flush.AfterItemID) {
		t.Fatalf("flush = %+v", flush)
	}
}

func TestManualModeAdmitsQuietAudioReopensSilenceAndCommitsExplicitly(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointManual})
	harness.send("audio", "manual-quiet", input("manual", audioFrame("microphone", 1, 0)))
	quiet := payload[perceptionelements.AudioBatch](t, receive(t, harness.output("admitted")))
	if quiet.StreamID != "manual" || len(quiet.Frames) != 1 || len(quiet.Frames[0].PCM16LE) != audioFrameBytes {
		t.Fatalf("manual quiet admission = %+v", quiet)
	}
	assertNoEnvelope(t, harness.output("activity"), 50*time.Millisecond)

	// Open the physical VAD, then prove silence is explicitly reopened rather
	// than silently ending the client-owned turn.
	harness.send("audio", "manual-loud-1", input("manual", audioFrame("microphone", 2, 2_000)))
	harness.send("audio", "manual-loud-2", input("manual", audioFrame("microphone", 3, 2_000)))
	_ = receive(t, harness.output("activity"))
	// Every raw frame is admitted exactly once in manual mode.
	_ = receive(t, harness.output("admitted"))
	_ = receive(t, harness.output("admitted"))
	harness.send("audio", "manual-silence-1", input("manual", audioFrame("microphone", 4, 0)))
	harness.send("audio", "manual-silence-2", input("manual", audioFrame("microphone", 5, 0)))
	_ = receive(t, harness.output("admitted"))
	_ = receive(t, harness.output("admitted"))
	candidate := payload[EndpointCandidate](t, receive(t, harness.output("candidate")))
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.Action != GateReopen || command.CandidateID != candidate.ID {
		t.Fatalf("manual silence command = %+v", command)
	}
	assertNoEnvelope(t, harness.output("flush"), 50*time.Millisecond)

	harness.send("commit", "manual-commit", AudioCommit{StreamID: "manual"})
	force := payload[GateCommand](t, receive(t, harness.output("command")))
	if force.Action != GateForceClose || force.StreamID != "manual" {
		t.Fatalf("manual commit command = %+v", force)
	}
	flushEnvelope := receive(t, harness.output("flush"))
	flush := payload[perceptionelements.Flush](t, flushEnvelope)
	if flush.StreamID != "manual" || flush.AfterItemID == "" ||
		!slices.Contains(flushEnvelope.CausalParents, flush.AfterItemID) {
		t.Fatalf("manual flush = %+v", flush)
	}
}

func TestManualEmptyCommitIsRefusedByAdmission(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointManual})
	harness.send("commit", "empty-commit", AudioCommit{StreamID: "empty"})
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.Action != GateForceClose {
		t.Fatalf("command = %+v", command)
	}
	outcome := awaitAdmissionOutcome(t, harness, "empty-commit", "empty_stream")
	if outcome.Kind != OutcomeRefused {
		t.Fatalf("empty commit outcome = %+v", outcome)
	}
	assertNoEnvelope(t, harness.output("flush"), 75*time.Millisecond)
}

func TestExternalVerdictAndExplicitTickFallback(t *testing.T) {
	t.Run("verdict reopens", func(t *testing.T) {
		harness := mountAcoustic(t, EndpointPolicyConfig{
			Mode: EndpointExternal, CandidateTimeoutMS: 100, Fallback: GateClose,
		})
		candidate := reachCandidate(t, harness, "verdict", 1_000_000_000)
		harness.send("verdict", "external-verdict", EndpointVerdict{
			CandidateID: candidate.ID, StreamID: candidate.StreamID, Action: GateReopen,
		})
		command := payload[GateCommand](t, receive(t, harness.output("command")))
		if command.Action != GateReopen || command.CandidateID != candidate.ID {
			t.Fatalf("verdict command = %+v", command)
		}
		assertNoEnvelope(t, harness.output("flush"), 50*time.Millisecond)
	})

	t.Run("tick closes at deadline", func(t *testing.T) {
		harness := mountAcoustic(t, EndpointPolicyConfig{
			Mode: EndpointExternal, CandidateTimeoutMS: 100, Fallback: GateClose,
		})
		candidate := reachCandidate(t, harness, "deadline", 2_000_000_000)
		awaitEndpointOutcome(t, harness, "deadline-frame-4:candidate", "awaiting_verdict_or_tick")
		harness.send("tick", "tick-start", TimingTick{NowNS: candidate.DetectedNS})
		started := awaitEndpointOutcome(t, harness, "tick-start", "deadline_started")
		if started.Kind != OutcomePending {
			t.Fatalf("deadline start outcome = %+v", started)
		}
		harness.send("tick", "tick-early", TimingTick{NowNS: candidate.DetectedNS + 99_000_000})
		outcome := awaitEndpointOutcome(t, harness, "tick-early", "deadline_not_reached")
		if outcome.Kind != OutcomePending {
			t.Fatalf("early tick outcome = %+v", outcome)
		}
		assertNoEnvelope(t, harness.output("command"), 50*time.Millisecond)
		harness.send("tick", "tick-due", TimingTick{NowNS: candidate.DetectedNS + 100_000_000})
		command := payload[GateCommand](t, receive(t, harness.output("command")))
		if command.Action != GateClose || command.CandidateID != candidate.ID {
			t.Fatalf("deadline command = %+v", command)
		}
		_ = receive(t, harness.output("flush"))
	})
}

func TestExternalDeadlineRejectsBackwardTicksAndDoesNotMixCaptureWithTickTime(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{
		Mode: EndpointExternal, CandidateTimeoutMS: 100, Fallback: GateClose,
	})
	candidate := reachCandidate(t, harness, "clock-domain", 9_000_000_000)
	awaitEndpointOutcome(t, harness, "clock-domain-frame-4:candidate", "awaiting_verdict_or_tick")

	// Capture timestamps are observation evidence, not a deadline clock. The
	// first explicit tick establishes the policy deadline in its own monotonic
	// domain even when that domain starts far below CapturedNS.
	harness.send("tick", "domain-start", TimingTick{NowNS: 10})
	awaitEndpointOutcome(t, harness, "domain-start", "deadline_started")
	harness.send("tick", "domain-backward", TimingTick{NowNS: 9})
	stale := awaitEndpointOutcome(t, harness, "domain-backward", "stale_tick")
	if stale.Kind != OutcomeIgnored {
		t.Fatalf("backward tick outcome = %+v", stale)
	}
	harness.send("tick", "domain-due", TimingTick{NowNS: 100_000_010})
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.CandidateID != candidate.ID || command.Action != GateClose {
		t.Fatalf("domain-safe deadline command = %+v", command)
	}
}

func TestCancellationBeforeAudioAndCandidateIsRemembered(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointExternal, CandidateTimeoutMS: 100})
	harness.send("cancel", "cancel-before", perceptionelements.Cancel{StreamID: "future", Reason: "test"})
	_ = receive(t, harness.output("cancel_downstream"))
	awaitAdmissionOutcome(t, harness, "cancel-before", "canceled")
	awaitEndpointOutcome(t, harness, "cancel-before", "canceled")

	harness.send("audio", "late-audio", input("future", audioFrame("microphone", 1, 2_000)))
	outcome := awaitAdmissionOutcome(t, harness, "late-audio", "canceled_before_audio")
	if outcome.Kind != OutcomeCanceled {
		t.Fatalf("late audio outcome = %+v", outcome)
	}
	assertNoEnvelope(t, harness.output("admitted"), 50*time.Millisecond)

	// The endpoint half independently fences a candidate that races in after
	// its cancellation, even if it was not produced by this admission node.
	candidatePort := directEndpointHarness(t, EndpointPolicyConfig{
		Mode: EndpointExternal, CandidateTimeoutMS: 100,
	})
	candidatePort.sendCancel("pre-candidate", "canceled-candidate")
	candidatePort.sendCandidate("late-candidate", EndpointCandidate{
		ID: "candidate-1", StreamID: "canceled-candidate", Source: "microphone",
		SampleRateHz: sampleRate, Sequence: 1,
	})
	late := candidatePort.receiveOutcome("late-candidate")
	if late.Kind != OutcomeCanceled || late.Code != "canceled_before_candidate" {
		t.Fatalf("late candidate outcome = %+v", late)
	}
}

func TestExternalVerdictCanArriveBeforeItsCandidate(t *testing.T) {
	harness := directEndpointHarness(t, EndpointPolicyConfig{
		Mode: EndpointExternal, CandidateTimeoutMS: 100,
	})
	verdict := EndpointVerdict{
		CandidateID: "future-candidate", StreamID: "future-stream", Action: GateClose,
	}
	sendPort(t, harness.verdict, "early-verdict", verdict)
	early := harness.receiveOutcome("early-verdict")
	if early.Kind != OutcomePending || early.Code != "awaiting_candidate" {
		t.Fatalf("early verdict outcome = %+v", early)
	}
	_ = receive(t, harness.state)
	harness.sendCandidate("future-candidate-envelope", EndpointCandidate{
		ID: "future-candidate", StreamID: "future-stream", Source: "microphone",
		SampleRateHz: sampleRate, Sequence: 1,
	})
	command := payload[GateCommand](t, receive(t, harness.command))
	if command.Action != GateClose || command.CandidateID != verdict.CandidateID {
		t.Fatalf("reordered verdict command = %+v", command)
	}
	resolved := harness.receiveOutcome("early-verdict:candidate-arrived")
	if resolved.Kind != OutcomeSucceeded || resolved.Code != "external_verdict" {
		t.Fatalf("reordered verdict resolution = %+v", resolved)
	}
}

func TestSourceSampleRateMalformedFramesAndBoundedConfiguration(t *testing.T) {
	harness := mountAcousticWithConfig(t, EndpointPolicyConfig{Mode: EndpointAutomatic}, admissionTestConfig("microphone"))
	harness.send("audio", "wrong-source", input("source", audioFrame("camera", 1, 2_000)))
	if outcome := awaitAdmissionOutcome(t, harness, "wrong-source", "invalid_frame"); !strings.Contains(outcome.Message, "configured source") {
		t.Fatalf("wrong source outcome = %+v", outcome)
	}
	harness.send("audio", "valid-rate", input("source", audioFrame("microphone", 2, 0)))
	frame := audioFrame("microphone", 3, 0)
	frame.SampleRateHz = sampleRate * 2
	harness.send("audio", "new-rate-quiet", input("source", frame))
	if outcome := awaitAdmissionOutcome(t, harness, "new-rate-quiet", "observed"); outcome.Kind != OutcomeSucceeded {
		t.Fatalf("quiet rate rebuild outcome = %+v", outcome)
	}
	// Once audio was admitted, a rate transition is an explicit refusal rather
	// than an invisible reset in the middle of a turn.
	harness.send("audio", "rate-loud-1", input("source", frameWithRate("microphone", 4, 2_000, sampleRate*2)))
	harness.send("audio", "rate-loud-2", input("source", frameWithRate("microphone", 5, 2_000, sampleRate*2)))
	_ = receive(t, harness.output("activity"))
	_ = receive(t, harness.output("admitted"))
	harness.send("audio", "rate-change-active", input("source", audioFrame("microphone", 6, 2_000)))
	if outcome := awaitAdmissionOutcome(t, harness, "rate-change-active", "sample_rate_change_active"); outcome.Kind != OutcomeRefused {
		t.Fatalf("active rate change outcome = %+v", outcome)
	}

	if _, err := decodeAdmissionConfig(json.RawMessage(`{"terminal_memory":4097}`)); err == nil {
		t.Fatal("expected bounded terminal memory validation failure")
	}
	if _, err := decodeAdmissionConfig(json.RawMessage(`{"max_frame_bytes":33554433}`)); err == nil {
		t.Fatal("expected bounded frame validation failure")
	}
	if _, err := decodeEndpointConfig(json.RawMessage(`{"mode":"external","candidate_timeout_ms":0}`)); err == nil {
		t.Fatal("expected external timeout validation failure")
	}
	memory := newBoundedSet(2)
	memory.Add("one")
	memory.Add("two")
	memory.Add("three")
	if memory.Len() != 2 || memory.Has("one") || !memory.Has("two") || !memory.Has("three") {
		t.Fatalf("bounded memory retained %v", memory.order)
	}
	if err := validateIdentifier("ID", " spaced ", true); err == nil {
		t.Fatal("identifier with surrounding whitespace was accepted")
	}
	if err := validateIdentifier("ID", "line\nbreak", true); err == nil {
		t.Fatal("identifier with a control character was accepted")
	}
	if got := candidateIdentifier("node", strings.Repeat("s", maximumIdentifierLength), 1); len(got) > maximumIdentifierLength {
		t.Fatalf("generated candidate ID has %d bytes", len(got))
	}
	if got := boundedReason(strings.Repeat("r", 2048)); len(got) > 1024 || !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded reason has %d bytes", len(got))
	}
}

func TestLiveResolutionAttestsExactBuiltInsAndCompleteEmptyCapabilities(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})
	live := harness.mounted.Live()
	want := map[string]string{
		"admission": admissionRuntimeID, "endpoint_policy": endpointRuntimeID,
	}
	for node, runtimeID := range want {
		resolution := live.Nodes[node].Resolution
		if resolution == nil {
			t.Fatalf("node %s has no resolution", node)
		}
		expectedRevision := endpointImplementationRevision
		if runtimeID == admissionRuntimeID {
			expectedRevision = admissionImplementationRevision
		}
		if resolution.Runtime.ID != runtimeID || resolution.Runtime.Revision != expectedRevision ||
			resolution.RuntimeEvidence != inspect.EvidenceLive {
			t.Fatalf("node %s runtime resolution = %+v", node, resolution)
		}
		if resolution.CapabilitiesEvidence != inspect.EvidenceLive || len(resolution.Capabilities) != 0 {
			t.Fatalf("node %s capability resolution = %+v", node, resolution)
		}
	}
}

const (
	sampleRate      = uint32(16_000)
	audioFrameMS    = 20
	audioFrameBytes = int(sampleRate) * audioFrameMS / 1_000 * 2
)

type acousticHarness struct {
	t       *testing.T
	mounted *graphruntime.Mounted
	inputs  map[string]element.OutputPort
	outputs map[string]element.InputPort
	cancel  context.CancelFunc
	done    chan error
	stop    sync.Once
}

func mountAcoustic(t *testing.T, endpoint EndpointPolicyConfig) *acousticHarness {
	return mountAcousticWithConfig(t, endpoint, admissionTestConfig(""))
}

func admissionTestConfig(source string) AdmissionConfig {
	threshold, prefix, silence, speech := 0.5, 20, 40, 40
	return AdmissionConfig{
		Threshold: &threshold, PrefixPaddingMS: &prefix, SilenceDurationMS: &silence,
		SpeechDurationMS: &speech, Source: source, CancellationMemory: 4,
		TerminalMemory: 4, MaxFrameBytes: 64 * 1024, MaxSampleRateHz: 64_000,
	}
}

func mountAcousticWithConfig(
	t *testing.T, endpoint EndpointPolicyConfig, admission AdmissionConfig,
) *acousticHarness {
	t.Helper()
	file, err := syntax.Parse("agent.ortg", readComponentFile(t, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(readComponentFile(t, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: acousticCatalog(t), Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	document := graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: "acoustic_endpoint",
		Nodes: map[string]json.RawMessage{
			"admission": mustJSON(t, admission), "endpoint_policy": mustJSON(t, endpoint),
			"policy_state_copy": json.RawMessage(`{}`), "candidate_copy": json.RawMessage(`{}`),
			"command_copy": json.RawMessage(`{}`), "cancellation_copy": json.RawMessage(`{}`),
		},
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := flow.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	if err := RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Values: bound.Values,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &acousticHarness{
		t: t, mounted: mounted, inputs: make(map[string]element.OutputPort),
		outputs: make(map[string]element.InputPort), done: make(chan error, 1),
	}
	for _, name := range []string{"audio", "tick", "commit", "verdict", "cancel"} {
		harness.inputs[name], err = mounted.Ingress(name)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"admitted", "flush", "activity", "candidate", "command", "admission_state",
		"policy_state", "admission_outcome", "endpoint_outcome", "cancel_downstream",
	} {
		harness.outputs[name], err = mounted.Egress(name)
		if err != nil {
			t.Fatal(err)
		}
	}
	runContext, cancelRun := context.WithCancel(context.Background())
	harness.cancel = cancelRun
	go func() { harness.done <- mounted.Run(runContext) }()
	t.Cleanup(harness.close)
	harness.awaitReady(endpoint.Mode)
	return harness
}

func (harness *acousticHarness) awaitReady(mode EndpointMode) {
	harness.t.Helper()
	if mode == "" {
		mode = EndpointAutomatic
	}
	policy := payload[EndpointPolicyState](harness.t, receive(harness.t, harness.output("policy_state")))
	if policy.Mode != mode || policy.Sequence == 0 {
		harness.t.Fatalf("initial policy state = %+v", policy)
	}
	for attempts := 0; attempts < 4; attempts++ {
		state := payload[AdmissionState](harness.t, receive(harness.t, harness.output("admission_state")))
		if state.Phase == AdmissionIdle && state.Mode == mode {
			awaitAdmissionOutcome(harness.t, harness, "endpoint_policy:state:1", "policy_applied")
			return
		}
	}
	harness.t.Fatal("admission did not consume initial endpoint policy state")
}

func (harness *acousticHarness) close() {
	harness.stop.Do(func() {
		harness.cancel()
		select {
		case err := <-harness.done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, graphruntime.ErrGraphClosed) {
				harness.t.Errorf("graph shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			harness.t.Error("graph did not stop")
		}
	})
}

func (harness *acousticHarness) send(name, itemID string, value any) {
	harness.t.Helper()
	port := harness.inputs[name]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := port.Broadcast(ctx, element.Envelope{
		Type: port.Type(), ItemID: itemID, Payload: value,
	}); err != nil {
		harness.t.Fatalf("send %s: %v", name, err)
	}
}

func (harness *acousticHarness) output(name string) element.InputPort {
	harness.t.Helper()
	port, found := harness.outputs[name]
	if !found {
		harness.t.Fatalf("unknown output %q", name)
	}
	return port
}

func acousticCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	if err := flow.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func readComponentFile(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "graphs", "components", "acoustic-endpoint", name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func findNode(t *testing.T, graph ir.Graph, id string) ir.Node {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("node %q not found", id)
	return ir.Node{}
}

func receive(t *testing.T, port element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatalf("receive %s: %v", port.Name(), err)
	}
	return envelope
}

func assertNoEnvelope(t *testing.T, port element.InputPort, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	if envelope, err := port.Receive(ctx); err == nil {
		t.Fatalf("unexpected envelope on %s: %s", port.Name(), envelope.ItemID)
	}
}

func payload[T any](t *testing.T, envelope element.Envelope) T {
	t.Helper()
	value, ok := envelope.Payload.(T)
	if !ok {
		var zero T
		t.Fatalf("payload type = %T, want %T", envelope.Payload, zero)
	}
	return value
}

func input(streamID string, frame coreperception.Frame) InputFrame {
	return InputFrame{StreamID: streamID, Frame: frame}
}

func audioFrame(source string, index uint64, amplitude int16) coreperception.Frame {
	return frameWithRate(source, index, amplitude, sampleRate)
}

func frameWithRate(source string, index uint64, amplitude int16, rate uint32) coreperception.Frame {
	samples := int(rate) * audioFrameMS / 1_000
	pcm := make([]byte, samples*2)
	for offset := 0; offset < len(pcm); offset += 2 {
		binary.LittleEndian.PutUint16(pcm[offset:], uint16(amplitude))
	}
	return coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: source, CapturedNS: index * uint64(audioFrameMS) * 1_000_000,
		Index: index, PCM16LE: pcm, SampleRateHz: rate,
	}
}

func openSpeech(t *testing.T, harness *acousticHarness, stream string) {
	t.Helper()
	harness.send("audio", stream+"-loud-1", input(stream, audioFrame("microphone", 1, 2_000)))
	harness.send("audio", stream+"-loud-2", input(stream, audioFrame("microphone", 2, 2_000)))
	activity := payload[SpeechActivity](t, receive(t, harness.output("activity")))
	if activity.Kind != SpeechStarted {
		t.Fatalf("activity = %+v", activity)
	}
	_ = receive(t, harness.output("admitted"))
}

func reachCandidate(t *testing.T, harness *acousticHarness, stream string, baseNS uint64) EndpointCandidate {
	t.Helper()
	for index, amplitude := range []int16{2_000, 2_000, 0, 0} {
		frame := audioFrame("microphone", uint64(index+1), amplitude)
		frame.CapturedNS = baseNS + uint64(index)*uint64(audioFrameMS)*1_000_000
		harness.send("audio", stream+"-frame-"+string(rune('1'+index)), input(stream, frame))
	}
	_ = receive(t, harness.output("activity"))
	for range 3 {
		_ = receive(t, harness.output("admitted"))
	}
	return payload[EndpointCandidate](t, receive(t, harness.output("candidate")))
}

func awaitAdmissionOutcome(
	t *testing.T, harness *acousticHarness, causalPrefix, code string,
) AdmissionOutcome {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		envelope := receive(t, harness.output("admission_outcome"))
		outcome := payload[AdmissionOutcome](t, envelope)
		if strings.HasPrefix(envelope.ItemID, causalPrefix) && outcome.Code == code {
			return outcome
		}
	}
	t.Fatalf("admission outcome %s/%s not found", causalPrefix, code)
	return AdmissionOutcome{}
}

func awaitEndpointOutcome(
	t *testing.T, harness *acousticHarness, causalPrefix, code string,
) EndpointOutcome {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		envelope := receive(t, harness.output("endpoint_outcome"))
		outcome := payload[EndpointOutcome](t, envelope)
		if strings.HasPrefix(envelope.ItemID, causalPrefix) && outcome.Code == code {
			return outcome
		}
	}
	t.Fatalf("endpoint outcome %s/%s not found", causalPrefix, code)
	return EndpointOutcome{}
}

// endpointOnlyHarness isolates cancellation-before-candidate without adding a
// synthetic second producer to the reference graph's typed candidate edge.
type endpointOnlyHarness struct {
	t         *testing.T
	candidate element.OutputPort
	verdict   element.OutputPort
	cancel    element.OutputPort
	outcome   element.InputPort
	state     element.InputPort
	command   element.InputPort
	cancelRun context.CancelFunc
	done      chan error
}

func directEndpointHarness(t *testing.T, config EndpointPolicyConfig) *endpointOnlyHarness {
	t.Helper()
	const source = `graph endpoint_only {
    acoustic.EndpointPolicy :: endpoint;
    input candidate = endpoint.candidate;
    input tick = endpoint.tick;
    input commit = endpoint.commit;
    input verdict = endpoint.verdict;
    input cancel = endpoint.cancel;
    output command = endpoint.command;
    output state = endpoint.state;
    output outcome = endpoint.outcome;
}`
	file, err := syntax.Parse("endpoint.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: acousticCatalog(t), ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: "endpoint_only",
		Nodes: map[string]json.RawMessage{"endpoint": mustJSON(t, config)},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Values: bound.Values,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &endpointOnlyHarness{t: t, done: make(chan error, 1)}
	harness.candidate, _ = mounted.Ingress("candidate")
	harness.verdict, _ = mounted.Ingress("verdict")
	harness.cancel, _ = mounted.Ingress("cancel")
	harness.outcome, _ = mounted.Egress("outcome")
	harness.state, _ = mounted.Egress("state")
	harness.command, _ = mounted.Egress("command")
	runContext, cancelRun := context.WithCancel(context.Background())
	harness.cancelRun = cancelRun
	go func() { harness.done <- mounted.Run(runContext) }()
	_ = receive(t, harness.state)
	t.Cleanup(func() {
		cancelRun()
		select {
		case <-harness.done:
		case <-time.After(2 * time.Second):
			t.Error("endpoint-only graph did not stop")
		}
	})
	return harness
}

func (harness *endpointOnlyHarness) sendCancel(itemID, streamID string) {
	harness.t.Helper()
	sendPort(harness.t, harness.cancel, itemID, perceptionelements.Cancel{StreamID: streamID})
	_ = harness.receiveOutcome(itemID)
	_ = receive(harness.t, harness.state)
}

func (harness *endpointOnlyHarness) sendCandidate(itemID string, candidate EndpointCandidate) {
	harness.t.Helper()
	sendPort(harness.t, harness.candidate, itemID, candidate)
}

func (harness *endpointOnlyHarness) receiveOutcome(prefix string) EndpointOutcome {
	harness.t.Helper()
	for {
		envelope := receive(harness.t, harness.outcome)
		if strings.HasPrefix(envelope.ItemID, prefix) {
			return payload[EndpointOutcome](harness.t, envelope)
		}
	}
}

func sendPort(t *testing.T, port element.OutputPort, itemID string, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := port.Broadcast(ctx, element.Envelope{
		Type: port.Type(), ItemID: itemID, Payload: value,
	}); err != nil {
		t.Fatal(err)
	}
}
