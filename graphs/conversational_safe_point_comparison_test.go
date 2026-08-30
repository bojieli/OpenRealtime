package graphs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	conversationalSafePointFixture = "ordinary-conversational-turn/v1"
	conversationalUserText         = "hello from reference"
	conversationalFastText         = "fast response."
	conversationalSlowText         = "slow response."
	conversationalVoicedSlowText   = "voiced slow response."
	maximumSafePointArtifactBytes  = 128 << 10
)

type conversationalSafePointArtifact struct {
	FormatVersion uint64                              `json:"format_version"`
	Fixture       string                              `json:"fixture"`
	Cases         []conversationalSafePointComparison `json:"cases"`
	Fingerprint   string                              `json:"fingerprint"`
}

type conversationalSafePointComparison struct {
	Profile     string                      `json:"profile"`
	Equivalent  bool                        `json:"equivalent"`
	Differences []string                    `json:"differences"`
	Legacy      conversationalSafePointPath `json:"legacy"`
	GraphNative conversationalSafePointPath `json:"graph_native"`
}

type conversationalSafePointPath struct {
	Composition      string                    `json:"composition"`
	GraphFingerprint string                    `json:"graph_fingerprint"`
	SafePoints       []conversationalSafePoint `json:"safe_points"`
}

type conversationalSafePoint struct {
	Kind            string                              `json:"kind"`
	Role            string                              `json:"role,omitempty"`
	Ordinal         uint64                              `json:"ordinal"`
	Decision        string                              `json:"decision,omitempty"`
	Reason          string                              `json:"reason,omitempty"`
	Version         uint64                              `json:"version,omitempty"`
	SourceRevision  uint64                              `json:"source_revision,omitempty"`
	CrossedBoundary bool                                `json:"crossed_boundary,omitempty"`
	PlaybackOrder   uint64                              `json:"playback_order,omitempty"`
	Prefix          []conversationalSafePointPrefixItem `json:"prefix,omitempty"`
}

type conversationalSafePointPrefixItem struct {
	Kind           trajectory.Kind      `json:"kind"`
	Authority      trajectory.Authority `json:"authority"`
	SourceRevision uint64               `json:"source_revision,omitempty"`
}

type conversationalSafePointCase struct {
	profile             string
	legacyComposition   string
	legacyRollout       interaction.Rollout
	legacyFastOutputs   []string
	legacyPlaybackRoles []string
	graphPlaybackTexts  []string
	graphPlaybackRoles  []string
}

func TestConversationalSafePointComparisonIsRetainedAndPayloadSafe(t *testing.T) {
	want, raw := loadConversationalSafePointArtifact(t)
	if want.Fixture != conversationalSafePointFixture {
		t.Fatalf("safe-point artifact fixture = %q, want %q", want.Fixture, conversationalSafePointFixture)
	}
	for _, forbidden := range [][]byte{
		[]byte(conversationalUserText), []byte(conversationalFastText),
		[]byte(conversationalSlowText), []byte(conversationalVoicedSlowText),
	} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("safe-point artifact retained conversational payload %q", forbidden)
		}
	}

	cases := []conversationalSafePointCase{
		{
			profile: "conversational-fast-only", legacyComposition: "cascade/fast-only",
			legacyRollout:       interaction.NewFastOnlyRollout(),
			legacyFastOutputs:   []string{conversationalFastText},
			legacyPlaybackRoles: []string{"fast"},
			graphPlaybackTexts:  []string{conversationalFastText},
			graphPlaybackRoles:  []string{"fast"},
		},
		{
			profile: "conversational-slow-only", legacyComposition: "cascade/endpointed-slow-only",
			legacyRollout:       interaction.NewEndpointedSlowOnlyRollout(interaction.RolloutOptions{}),
			legacyFastOutputs:   []string{conversationalVoicedSlowText},
			legacyPlaybackRoles: []string{"fast"},
			graphPlaybackTexts:  []string{conversationalSlowText},
			graphPlaybackRoles:  []string{"deliberative"},
		},
		{
			profile:             "conversational-both",
			legacyComposition:   "cascade/fast-then-slow;deferral=comparison-safe-point",
			legacyRollout:       interaction.NewFastThenSlowRollout(interaction.RolloutOptions{}),
			legacyFastOutputs:   []string{conversationalFastText, conversationalVoicedSlowText},
			legacyPlaybackRoles: []string{"fast", "fast"},
			graphPlaybackTexts:  []string{conversationalFastText, conversationalSlowText},
			graphPlaybackRoles:  []string{"fast", "deliberative"},
		},
	}

	got := conversationalSafePointArtifact{
		FormatVersion: 1, Fixture: conversationalSafePointFixture,
		Cases: make([]conversationalSafePointComparison, 0, len(cases)),
	}
	for _, testCase := range cases {
		t.Run(testCase.profile, func(t *testing.T) {
			legacyPath := recordLegacyConversationalSafePoints(t, testCase)
			nativePath := recordGraphNativeConversationalSafePoints(t, testCase)
			assertConversationalSafePointCommonSlice(
				t, testCase.profile, legacyPath.SafePoints, nativePath.SafePoints,
			)
			differences := compareConversationalSafePoints(legacyPath.SafePoints, nativePath.SafePoints)
			if len(differences) == 0 {
				t.Fatal("safe-point comparison unexpectedly found exact parity; review and replace the retained mismatch artifact")
			}
			got.Cases = append(got.Cases, conversationalSafePointComparison{
				Profile: testCase.profile, Equivalent: false, Differences: differences,
				Legacy: legacyPath, GraphNative: nativePath,
			})
		})
	}
	got.Fingerprint = fingerprintConversationalSafePointArtifact(t, got)
	if !reflect.DeepEqual(got, want) {
		encoded, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("safe-point comparison drifted from retained actual traces\ngot:\n%s", encoded)
	}
}

func recordGraphNativeConversationalSafePoints(
	t *testing.T, testCase conversationalSafePointCase,
) conversationalSafePointPath {
	t.Helper()
	reference := loadConversationalReference(t, testCase.profile)
	fastTerminalGate := make(chan struct{})
	slowTerminalGate := make(chan struct{})
	var releaseFast, releaseSlow sync.Once
	control := &conversationalContinuationControl{
		terminalGates: map[trajectory.Phase]<-chan struct{}{
			trajectory.PhaseFast: fastTerminalGate,
			trajectory.PhaseSlow: slowTerminalGate,
		},
	}
	fixture := mountConversationalTurn(t, reference, control)
	defer func() {
		releaseSlow.Do(func() { close(slowTerminalGate) })
		releaseFast.Do(func() { close(fastTerminalGate) })
		fixture.stop(t)
	}()

	driveConversationalGraphAudio(t, fixture)
	observationEnvelope := fixture.await(t, "observation_commit_outcome", func(payload any) bool {
		outcome, ok := payload.(stateelements.ObservationCommitOutcome)
		return ok && outcome.Kind == stateelements.ObservationCommitted
	})
	observation := observationEnvelope.Payload.(stateelements.ObservationCommitOutcome)
	if observation.StoreVersion != 1 || observation.SourceRevision != 1 {
		t.Fatalf("graph-native observation safe point = %+v", observation)
	}

	fastBegin := fixture.await(t, "fast_prepared_text", preparedBoundary(cognitionelements.TextBegin))
	slowBegin := fixture.await(t, "slow_prepared_text", preparedBoundary(cognitionelements.TextBegin))
	if testCase.profile == "conversational-both" {
		fixture.sendSelection(t, fastBegin.RunID, interactionelements.SelectionPreempt, "parity-select-fast")
		fixture.sendSelection(t, slowBegin.RunID, interactionelements.SelectionQueue, "parity-queue-slow")
	}

	// Both providers have emitted prepared text against context v1, but neither
	// may report a terminal result yet. Let deliberative completion win the
	// shared compare-and-append deterministically, then release fast and observe
	// its refusal.
	releaseSlow.Do(func() { close(slowTerminalGate) })
	slowTerminalOutcome := fixture.awaitModelCommit(t, "slow_commit_outcome", slowBegin.RunID)
	if slowTerminalOutcome.Kind != interactionelements.ModelCommitted || slowTerminalOutcome.StoreVersion != 3 {
		t.Fatalf("graph-native deliberative terminal = %+v", slowTerminalOutcome)
	}
	releaseFast.Do(func() { close(fastTerminalGate) })
	fastTerminalOutcome := fixture.awaitModelCommit(t, "fast_commit_outcome", fastBegin.RunID)
	if fastTerminalOutcome.Kind != interactionelements.ModelRejected ||
		fastTerminalOutcome.Code != "version_conflict" || fastTerminalOutcome.StoreVersion != 3 {
		t.Fatalf("graph-native fast terminal = %+v", fastTerminalOutcome)
	}

	playbackOutcomes := make([]speechelements.PlaybackOutcome, 0, len(testCase.graphPlaybackTexts))
	for range testCase.graphPlaybackTexts {
		envelope := fixture.await(t, "playback_outcome", func(payload any) bool {
			outcome, ok := payload.(speechelements.PlaybackOutcome)
			return ok && (outcome.Kind == speechelements.OutcomeSucceeded ||
				outcome.Kind == speechelements.OutcomeCancelled)
		})
		playbackOutcomes = append(playbackOutcomes, envelope.Payload.(speechelements.PlaybackOutcome))
	}
	snapshotEnvelope := fixture.await(t, "trajectory_snapshot", func(payload any) bool {
		snapshot, ok := payload.(trajectory.Snapshot)
		return ok && snapshot.Version == 3
	})
	snapshot := snapshotEnvelope.Payload.(trajectory.Snapshot)
	assertConversationalSnapshotHasNoTools(t, snapshot)

	fastRequest := control.request(t, trajectory.PhaseFast, 0)
	slowRequest := control.request(t, trajectory.PhaseSlow, 0)
	assertParityRequestContent(t, fastRequest)
	assertParityRequestContent(t, slowRequest)

	fixture.sink.mu.Lock()
	playedTexts := make([]string, len(fixture.sink.begins))
	playedEnds := append([]action.Outcome(nil), fixture.sink.ends...)
	for index := range fixture.sink.begins {
		playedTexts[index] = fixture.sink.begins[index].Text
	}
	fixture.sink.mu.Unlock()
	if !reflect.DeepEqual(playedTexts, testCase.graphPlaybackTexts) {
		t.Fatalf("graph-native played text = %q, want %q", playedTexts, testCase.graphPlaybackTexts)
	}
	if len(playedEnds) != len(playbackOutcomes) {
		t.Fatalf("graph-native playback sink ended %d utterances, outcomes reported %d",
			len(playedEnds), len(playbackOutcomes))
	}

	points := []conversationalSafePoint{{
		Kind: "observation_commit", Ordinal: 1, Decision: "committed",
		Version: observation.StoreVersion, SourceRevision: observation.SourceRevision,
	}}
	points = append(points,
		projectContinuationPrefix(t, fastRequest, 1),
		projectContinuationPrefix(t, slowRequest, 1),
		projectGraphModelTerminal(t, trajectory.PhaseFast, 1, fastTerminalOutcome),
		projectGraphModelTerminal(t, trajectory.PhaseSlow, 1, slowTerminalOutcome),
	)
	roleOrdinals := make(map[string]uint64)
	for index, outcome := range playbackOutcomes {
		role := testCase.graphPlaybackRoles[index]
		roleOrdinals[role]++
		decision := "canceled"
		if outcome.Kind == speechelements.OutcomeSucceeded && outcome.CrossedBoundary && playedEnds[index].Completed {
			decision = "committed"
		}
		points = append(points, conversationalSafePoint{
			Kind: "assistant_playback", Role: role, Ordinal: roleOrdinals[role],
			Decision: decision, CrossedBoundary: outcome.CrossedBoundary,
			PlaybackOrder: uint64(index + 1),
		})
	}
	return conversationalSafePointPath{
		Composition: testCase.profile, GraphFingerprint: reference.bound.Graph.Fingerprint,
		SafePoints: canonicalConversationalSafePoints(points),
	}
}

func driveConversationalGraphAudio(t *testing.T, fixture *conversationalTurnFixture) {
	t.Helper()
	const sessionID = "session-parity"
	const streamID = "stream-parity"
	for index := uint64(1); index <= 31; index++ {
		amplitude := int16(0)
		if index <= 6 {
			amplitude = 2_000
		}
		fixture.send(t, "audio", element.Envelope{
			Type: fixture.boundaryType("audio"), ItemID: fmt.Sprintf("parity-audio-%d", index),
			SessionID: sessionID, SourceID: streamID, CancellationScope: streamID,
			Payload: acousticelements.InputFrame{
				StreamID: streamID, Frame: conversationalAudioFrame(index, amplitude),
			},
		})
	}
}

func projectGraphModelTerminal(
	t *testing.T, phase trajectory.Phase, ordinal uint64,
	outcome interactionelements.ModelCommitOutcome,
) conversationalSafePoint {
	t.Helper()
	point := conversationalSafePoint{
		Kind: "model_terminal", Role: conversationalRole(phase), Ordinal: ordinal,
		Version: outcome.StoreVersion,
	}
	switch outcome.Kind {
	case interactionelements.ModelCommitted:
		point.Decision = "adopted"
	case interactionelements.ModelRejected:
		point.Decision = "rejected"
		if outcome.Code != "version_conflict" {
			t.Fatalf("graph-native model rejection code = %q", outcome.Code)
		}
		point.Reason = "newer_canonical_prefix"
	default:
		t.Fatalf("graph-native model terminal kind = %q", outcome.Kind)
	}
	return point
}

func recordLegacyConversationalSafePoints(
	t *testing.T, testCase conversationalSafePointCase,
) conversationalSafePointPath {
	t.Helper()
	fast := &legacyParityContinuation{
		descriptor: conversationalFastDescriptor,
		outputs:    append([]string(nil), testCase.legacyFastOutputs...),
	}
	slowDescriptor := conversationalSlowDescriptor
	// Cascade's deliberative role owns executable tool authority even in this
	// tool-free fixture. The safe artifact records no provider configuration.
	slowDescriptor.ToolAuthority = continuation.ToolAuthorityExecute
	slow := &legacyParityContinuation{
		descriptor: slowDescriptor, outputs: []string{conversationalSlowText},
	}
	policies := interaction.Defaults()
	policies.Rollout = testCase.legacyRollout
	if testCase.profile == "conversational-both" {
		// Hold signal-only work after the initial cognition batch behind the
		// gate's public response-request wake. External visibility events still
		// commit unconditionally, so the test can release the actual background
		// result only after queued and played are both canonical facts.
		policies.Deferral = legacySafePointDeferral{}
	}
	underlying, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return &legacyParityASR{}, nil },
		Fast:       fast, Slow: slow, Speech: legacyParitySpeech{}, Policies: policies,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	sink := &legacyParitySink{ended: make(chan struct{}, 4)}
	live, err := wrapped.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "legacy-parity-" + testCase.profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if closeErr := live.Close(ctx, nil); closeErr != nil {
			t.Errorf("close legacy compatibility graph: %v", closeErr)
		}
	}()

	for index := uint64(1); index <= 31; index++ {
		amplitude := int16(0)
		if index <= 6 {
			amplitude = 2_000
		}
		if err := live.Audio(context.Background(), conversationalAudioFrame(index, amplitude)); err != nil {
			t.Fatalf("legacy audio frame %d: %v", index, err)
		}
	}
	playbackCompletions := len(testCase.legacyPlaybackRoles)
	if testCase.profile == "conversational-both" {
		select {
		case <-sink.ended:
		case <-time.After(10 * time.Second):
			t.Fatal("legacy compatibility turn did not complete its first playback")
		}
		playbackCompletions--
		stateDeadline := time.Now().Add(5 * time.Second)
		for {
			states := 0
			for _, item := range live.Trajectory().Items {
				if item.Kind == trajectory.KindAssistantState {
					states++
				}
			}
			if states == 2 {
				break
			}
			if time.Now().After(stateDeadline) {
				t.Fatalf("legacy first playback visibility states = %d, want 2 before background release", states)
			}
			time.Sleep(time.Millisecond)
		}
		if err := live.CreateResponse(context.Background()); err != nil {
			t.Fatalf("release legacy background-result safe point: %v", err)
		}
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for index := 0; index < playbackCompletions; index++ {
		select {
		case <-sink.ended:
		case <-deadline.C:
			t.Fatal("legacy compatibility turn did not reach playback completion")
		}
	}
	fastRequests := fast.retainedRequests()
	slowRequests := slow.retainedRequests()
	wantFast := len(testCase.legacyFastOutputs)
	wantSlow := 1
	if testCase.profile == "conversational-fast-only" {
		wantSlow = 0
	}
	if len(fastRequests) != wantFast || len(slowRequests) != wantSlow {
		t.Fatalf("legacy provider calls = fast %d slow %d, want fast %d slow profile-specific",
			len(fastRequests), len(slowRequests), wantFast)
	}
	for _, request := range append(append([]continuation.Request(nil), fastRequests...), slowRequests...) {
		assertParityRequestContent(t, request)
	}

	sink.mu.Lock()
	begins := append([]action.Utterance(nil), sink.begins...)
	ends := append([]action.Outcome(nil), sink.ends...)
	failures := append([]legacy.ErrorEvent(nil), sink.failures...)
	toolBatches := sink.toolBatches
	sink.mu.Unlock()
	if len(failures) != 0 || toolBatches != 0 {
		t.Fatalf("legacy compatibility output failures=%+v tool_batches=%d", failures, toolBatches)
	}
	if len(begins) != len(testCase.legacyFastOutputs) || len(ends) != len(begins) {
		t.Fatalf("legacy playback begins=%d ends=%d, want %d", len(begins), len(ends), len(testCase.legacyFastOutputs))
	}
	for index := range begins {
		if begins[index].Text != testCase.legacyFastOutputs[index] ||
			conversationalRole(begins[index].Phase) != testCase.legacyPlaybackRoles[index] {
			t.Fatalf("legacy playback %d = phase %q text %q", index, begins[index].Phase, begins[index].Text)
		}
	}
	wantAssistantStates := 0
	for _, utterance := range begins {
		// Cascade records the reversible queued state and the terminal played
		// state separately for every assistant item crossing playback.
		wantAssistantStates += 2 * len(utterance.AssistantItemIDs)
	}
	wantFinalVersion := uint64(1 + 2*(len(fastRequests)+len(slowRequests)) + wantAssistantStates)
	var snapshot trajectory.Snapshot
	snapshotDeadline := time.Now().Add(5 * time.Second)
	for {
		snapshot = live.Trajectory()
		states := 0
		for _, item := range snapshot.Items {
			if item.Kind == trajectory.KindAssistantState {
				states++
			}
		}
		if states == wantAssistantStates && snapshot.Version == wantFinalVersion {
			break
		}
		if time.Now().After(snapshotDeadline) {
			t.Fatalf("legacy playback state did not settle: version=%d want=%d states=%d want=%d",
				snapshot.Version, wantFinalVersion, states, wantAssistantStates)
		}
		time.Sleep(time.Millisecond)
	}
	assertConversationalSnapshotHasNoTools(t, snapshot)

	observationIndex := -1
	var observation trajectory.Item
	for index, item := range snapshot.Items {
		if item.Kind == trajectory.KindObservation {
			if observationIndex >= 0 {
				t.Fatal("legacy ordinary turn committed multiple observations")
			}
			observationIndex, observation = index, item
		}
	}
	if observationIndex != 0 || observation.Content != conversationalUserText || observation.SourceRevision != 1 {
		t.Fatalf("legacy committed observation = index %d %+v", observationIndex, observation)
	}
	points := []conversationalSafePoint{{
		Kind: "observation_commit", Ordinal: 1, Decision: "committed",
		Version: uint64(observationIndex + 1), SourceRevision: observation.SourceRevision,
	}}
	for index, request := range fastRequests {
		ordinal := uint64(index + 1)
		points = append(points, projectContinuationPrefix(t, request, ordinal))
		points = append(points, projectLegacyModelTerminal(t, snapshot, request, ordinal))
	}
	for index, request := range slowRequests {
		ordinal := uint64(index + 1)
		points = append(points, projectContinuationPrefix(t, request, ordinal))
		points = append(points, projectLegacyModelTerminal(t, snapshot, request, ordinal))
	}
	playbackOrdinals := make(map[string]uint64)
	for index, utterance := range begins {
		role := conversationalRole(utterance.Phase)
		playbackOrdinals[role]++
		decision := "canceled"
		if ends[index].Completed {
			decision = "committed"
		}
		points = append(points, conversationalSafePoint{
			Kind: "assistant_playback", Role: role, Ordinal: playbackOrdinals[role],
			Decision: decision, CrossedBoundary: ends[index].PlayedMS > 0,
			PlaybackOrder: uint64(index + 1),
		})
	}
	status := live.Status()
	if status.Graph.Fingerprint != wrapped.Graph().Fingerprint {
		t.Fatalf("legacy live graph fingerprint = %q, mounted %q",
			status.Graph.Fingerprint, wrapped.Graph().Fingerprint)
	}
	return conversationalSafePointPath{
		Composition: testCase.legacyComposition, GraphFingerprint: wrapped.Graph().Fingerprint,
		SafePoints: canonicalConversationalSafePoints(points),
	}
}

// legacySafePointDeferral gives the comparison fixture a deterministic public
// release for cascade's signal-only background-result batch. It does not gate
// observations or deliberation and cannot prevent assistant visibility from
// committing: eventloop commit happens before Admit. Requested is set and
// woken through Live.CreateResponse, the same payload-free gate seam used by a
// client-driven session.
type legacySafePointDeferral struct{}

func (legacySafePointDeferral) Name() string { return "comparison-safe-point" }

func (legacySafePointDeferral) Conditions() []session.TransitionKind { return nil }

func (legacySafePointDeferral) Admit(waiting interaction.Waiting) (bool, string) {
	if waiting.Observation || waiting.ToolResult || waiting.Repair || waiting.Deliberation || waiting.Requested {
		return true, ""
	}
	return false, "comparison waits for an explicit safe-point release"
}

func projectContinuationPrefix(
	t *testing.T, request continuation.Request, ordinal uint64,
) conversationalSafePoint {
	t.Helper()
	items := request.Trajectory.Items
	version := request.Trajectory.Version
	// The legacy Runner exposes the current invocation instruction as a
	// provider-visible, uncommitted tail item. Graph-native TextModel carries
	// that same current instruction in Request.Invocation. Provider adapters
	// explicitly ignore historical instruction items when rendering context.
	// Remove only the exact current invocation tail to project the shared
	// immutable canonical prefix; content is checked in memory and never kept.
	if len(items) > 0 {
		last := items[len(items)-1]
		if last.Kind == trajectory.KindInstruction && last.InvocationID == request.InvocationID &&
			last.Content == request.Invocation.Instruction {
			if version == 0 {
				t.Fatal("current invocation instruction underflowed the provider prefix")
			}
			items, version = items[:len(items)-1], version-1
		}
	}
	if uint64(len(items)) != version {
		t.Fatalf("canonical continuation prefix version=%d items=%d", version, len(items))
	}
	prefix := make([]conversationalSafePointPrefixItem, len(items))
	for index, item := range items {
		prefix[index] = conversationalSafePointPrefixItem{
			Kind: item.Kind, Authority: trajectory.AuthorityOf(item),
			SourceRevision: item.SourceRevision,
		}
	}
	return conversationalSafePoint{
		Kind: "continuation_prefix", Role: conversationalRole(request.Descriptor.Phase),
		Ordinal: ordinal, Version: version, SourceRevision: request.Invocation.SourceRevision,
		Prefix: prefix,
	}
}

func projectLegacyModelTerminal(
	t *testing.T, snapshot trajectory.Snapshot, request continuation.Request, ordinal uint64,
) conversationalSafePoint {
	t.Helper()
	var terminalVersion uint64
	var instruction, assistant bool
	for index, item := range snapshot.Items {
		if item.InvocationID != request.InvocationID {
			continue
		}
		terminalVersion = uint64(index + 1)
		instruction = instruction || item.Kind == trajectory.KindInstruction
		assistant = assistant || item.Kind == trajectory.KindAssistant
	}
	if !instruction || !assistant || terminalVersion == 0 {
		t.Fatalf("legacy model invocation %q lacks a committed instruction/assistant terminal", request.InvocationID)
	}
	return conversationalSafePoint{
		Kind: "model_terminal", Role: conversationalRole(request.Descriptor.Phase),
		Ordinal: ordinal, Decision: "adopted", Version: terminalVersion,
	}
}

func assertParityRequestContent(t *testing.T, request continuation.Request) {
	t.Helper()
	var observations int
	for _, item := range request.Trajectory.Items {
		if item.Kind != trajectory.KindObservation {
			continue
		}
		observations++
		if item.Content != conversationalUserText || item.SourceRevision != 1 {
			t.Fatalf("provider received different observation payload/revision: %+v", item)
		}
	}
	if observations != 1 || request.Invocation.SourceRevision != 1 {
		t.Fatalf("provider request observations=%d source_revision=%d", observations, request.Invocation.SourceRevision)
	}
}

func assertConversationalSnapshotHasNoTools(t *testing.T, snapshot trajectory.Snapshot) {
	t.Helper()
	for _, item := range snapshot.Items {
		switch item.Kind {
		case trajectory.KindToolCall, trajectory.KindToolProposal, trajectory.KindToolResult:
			t.Fatalf("tool-free ordinary fixture produced %s", item.Kind)
		}
	}
}

func conversationalRole(phase trajectory.Phase) string {
	if phase == trajectory.PhaseSlow {
		return "deliberative"
	}
	return "fast"
}

func canonicalConversationalSafePoints(points []conversationalSafePoint) []conversationalSafePoint {
	result := append([]conversationalSafePoint(nil), points...)
	rank := map[string]int{
		"observation_commit": 0, "continuation_prefix": 1,
		"model_terminal": 2, "assistant_playback": 3, "tool_batch": 4,
	}
	sort.Slice(result, func(left, right int) bool {
		one, two := result[left], result[right]
		if rank[one.Kind] != rank[two.Kind] {
			return rank[one.Kind] < rank[two.Kind]
		}
		if one.Role != two.Role {
			return one.Role < two.Role
		}
		return one.Ordinal < two.Ordinal
	})
	return result
}

func compareConversationalSafePoints(
	legacyPoints, nativePoints []conversationalSafePoint,
) []string {
	legacyByKey := indexConversationalSafePoints(legacyPoints)
	nativeByKey := indexConversationalSafePoints(nativePoints)
	keys := make(map[string]struct{}, len(legacyByKey)+len(nativeByKey))
	for key := range legacyByKey {
		keys[key] = struct{}{}
	}
	for key := range nativeByKey {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var differences []string
	for _, key := range ordered {
		legacyPoint, hasLegacy := legacyByKey[key]
		nativePoint, hasNative := nativeByKey[key]
		switch {
		case !hasLegacy:
			differences = append(differences, "only_graph_native:"+key)
		case !hasNative:
			differences = append(differences, "only_legacy:"+key)
		case !reflect.DeepEqual(legacyPoint, nativePoint):
			differences = append(differences, "different:"+key)
		}
	}
	return differences
}

func assertConversationalSafePointCommonSlice(
	t *testing.T, profile string,
	legacyPoints, nativePoints []conversationalSafePoint,
) {
	t.Helper()
	legacyByKey := indexConversationalSafePoints(legacyPoints)
	nativeByKey := indexConversationalSafePoints(nativePoints)
	keys := []string{"observation_commit//1"}
	switch profile {
	case "conversational-fast-only":
		keys = append(keys, "continuation_prefix/fast/1", "assistant_playback/fast/1")
	case "conversational-slow-only":
		keys = append(keys, "continuation_prefix/deliberative/1", "model_terminal/deliberative/1")
	case "conversational-both":
		keys = append(keys, "continuation_prefix/fast/1", "assistant_playback/fast/1")
	default:
		t.Fatalf("unknown conversational comparison profile %q", profile)
	}
	for _, key := range keys {
		legacyPoint, legacyFound := legacyByKey[key]
		nativePoint, nativeFound := nativeByKey[key]
		if !legacyFound || !nativeFound || !reflect.DeepEqual(legacyPoint, nativePoint) {
			t.Fatalf("declared common safe point %s differs: legacy=%+v native=%+v",
				key, legacyPoint, nativePoint)
		}
	}
}

func indexConversationalSafePoints(points []conversationalSafePoint) map[string]conversationalSafePoint {
	result := make(map[string]conversationalSafePoint, len(points))
	for _, point := range points {
		key := fmt.Sprintf("%s/%s/%d", point.Kind, point.Role, point.Ordinal)
		if _, duplicate := result[key]; duplicate {
			panic("duplicate conversational safe point " + key)
		}
		result[key] = point
	}
	return result
}

func loadConversationalSafePointArtifact(
	t *testing.T,
) (conversationalSafePointArtifact, []byte) {
	t.Helper()
	payload := conversationalRead(t, "testdata/conversational_safe_point_comparison.json")
	if len(payload) == 0 || len(payload) > maximumSafePointArtifactBytes {
		t.Fatalf("safe-point artifact bytes = %d, bound %d", len(payload), maximumSafePointArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var document conversationalSafePointArtifact
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode safe-point artifact: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("safe-point artifact trailing data: %v", err)
	}
	if document.FormatVersion != 1 || len(document.Cases) != 3 {
		t.Fatalf("safe-point artifact header = version %d cases %d", document.FormatVersion, len(document.Cases))
	}
	if got := fingerprintConversationalSafePointArtifact(t, document); got != document.Fingerprint {
		t.Fatalf("safe-point artifact fingerprint = %q, computed %q", document.Fingerprint, got)
	}
	for index, profile := range []string{
		"conversational-fast-only", "conversational-slow-only", "conversational-both",
	} {
		candidate := document.Cases[index]
		if candidate.Profile != profile || len(candidate.Profile) > 64 || candidate.Equivalent ||
			len(candidate.Differences) == 0 || len(candidate.Differences) > 32 {
			t.Fatalf("safe-point artifact case %d = %+v", index, candidate)
		}
		for _, difference := range candidate.Differences {
			if difference == "" || len(difference) > 128 {
				t.Fatalf("safe-point artifact has invalid difference %q", difference)
			}
		}
		validateConversationalSafePointPath(t, candidate.Legacy)
		validateConversationalSafePointPath(t, candidate.GraphNative)
	}
	return document, payload
}

func validateConversationalSafePointPath(t *testing.T, path conversationalSafePointPath) {
	t.Helper()
	if path.Composition == "" || len(path.Composition) > 128 ||
		!validSafePointFingerprint(path.GraphFingerprint) ||
		len(path.SafePoints) == 0 || len(path.SafePoints) > 32 {
		t.Fatalf("invalid bounded safe-point path: %+v", path)
	}
	seen := make(map[string]struct{}, len(path.SafePoints))
	for _, point := range path.SafePoints {
		if len(point.Kind) > 64 || len(point.Role) > 32 || len(point.Decision) > 32 ||
			len(point.Reason) > 64 || point.Ordinal == 0 || len(point.Prefix) > 16 {
			t.Fatalf("invalid bounded safe point: %+v", point)
		}
		key := fmt.Sprintf("%s/%s/%d", point.Kind, point.Role, point.Ordinal)
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("safe-point artifact repeats %s", key)
		}
		seen[key] = struct{}{}
	}
}

func fingerprintConversationalSafePointArtifact(
	t *testing.T, document conversationalSafePointArtifact,
) string {
	t.Helper()
	document.Fingerprint = ""
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validSafePointFingerprint(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

type legacyParityASR struct{}

func (*legacyParityASR) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalASRDescriptor)
}

func (*legacyParityASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}

func (*legacyParityASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{
		RevisionID: 1, StableText: conversationalUserText, Final: true,
	}, nil
}

type legacyParityContinuation struct {
	descriptor continuation.Descriptor
	outputs    []string
	mu         sync.Mutex
	requests   []continuation.Request
}

func (provider *legacyParityContinuation) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *legacyParityContinuation) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.outputs) {
		provider.mu.Unlock()
		return continuation.Completion{}, fmt.Errorf("unexpected %s invocation %d", provider.descriptor.Phase, index+1)
	}
	text := provider.outputs[index]
	provider.mu.Unlock()
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: text}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func (provider *legacyParityContinuation) retainedRequests() []continuation.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]continuation.Request(nil), provider.requests...)
}

type legacyParitySpeech struct{}

func (legacyParitySpeech) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalTTSDescriptor)
}

func (legacyParitySpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("legacy parity fixture requires streaming synthesis")
}

func (legacyParitySpeech) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}
	return emit(v1.SpeechChunk{
		ChunkID: "legacy-parity-chunk", CandidateID: plan.CandidateID,
		// A realistic 200 ms utterance spans two 100 ms wire frames, keeping the
		// background-result signal behind the active speech gate for one pacing
		// interval. The previous 2 ms stub made the second legacy prefix depend
		// on which goroutine crossed the playback terminal first.
		SampleRateHz: 16_000, PCM16LE: make([]byte, 6_400), Final: true,
	})
}

type legacyParitySink struct {
	mu          sync.Mutex
	begins      []action.Utterance
	ends        []action.Outcome
	failures    []legacy.ErrorEvent
	toolBatches int
	ended       chan struct{}
}

func (*legacyParitySink) TurnBegin(context.Context) error                           { return nil }
func (*legacyParitySink) TurnEnd(context.Context, legacy.TurnOutcome) error         { return nil }
func (*legacyParitySink) Activity(context.Context, legacy.ActivityEvent) error      { return nil }
func (*legacyParitySink) Transcript(context.Context, legacy.TranscriptEvent) error  { return nil }
func (*legacyParitySink) Observation(context.Context, perception.Observation) error { return nil }

func (sink *legacyParitySink) SpeechBegin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	sink.begins = append(sink.begins, utterance)
	sink.mu.Unlock()
	return nil
}

func (*legacyParitySink) SpeechText(context.Context, action.Utterance, string) error { return nil }
func (*legacyParitySink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}

func (sink *legacyParitySink) SpeechEnd(
	_ context.Context, _ action.Utterance, outcome action.Outcome,
) error {
	sink.mu.Lock()
	sink.ends = append(sink.ends, outcome)
	sink.mu.Unlock()
	select {
	case sink.ended <- struct{}{}:
	default:
	}
	return nil
}

func (sink *legacyParitySink) ToolCalls(context.Context, legacy.ToolCallEvent) error {
	sink.mu.Lock()
	sink.toolBatches++
	sink.mu.Unlock()
	return nil
}

func (sink *legacyParitySink) Failed(_ context.Context, event legacy.ErrorEvent) {
	sink.mu.Lock()
	sink.failures = append(sink.failures, event)
	sink.mu.Unlock()
}
